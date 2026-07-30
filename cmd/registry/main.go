/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Command registry is Agent Plane's Data-Plane configuration endpoint. Agent
// runtimes talk to the Registry — never to the Kubernetes API server directly
// (design §14/§16). Given an Agent name, the Registry returns the fully
// resolved runtime configuration the Operator has assembled, and can stream
// updates so runtimes hot-reload when that configuration changes.
//
// Layering: the Registry watches only Agent objects. Dependency changes
// (Model, Tool, …) are propagated by the Operator into the Agent's
// status.resolvedConfigHash, so watching Agents is sufficient to observe any
// change that affects an agent's runtime config. gRPC and event-bus fan-out
// remain documented follow-ups.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	toolscache "k8s.io/client-go/tools/cache"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	sdk "github.com/hkmdxlftjf/agent-plane-sdk-go"

	corev1alpha1 "github.com/hkmdxlftjf/agent-plane/api/v1alpha1"
)

var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(corev1alpha1.AddToScheme(scheme))
}

// The payload runtimes consume is sdk.AgentConfig — the wire contract lives in
// the SDK (github.com/hkmdxlftjf/agent-plane-sdk-go) so the server and every
// runtime compile against the same types and cannot drift.

func main() {
	var addr string
	flag.StringVar(&addr, "addr", ":9090", "address to serve the Registry HTTP API on")
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseDevMode(true)))
	log := ctrl.Log.WithName("registry")
	ctx := ctrl.SetupSignalHandler()

	cfg, err := ctrl.GetConfig()
	if err != nil {
		log.Error(err, "unable to load kubeconfig / in-cluster config")
		os.Exit(1)
	}

	// Point reads use a direct client (no local cache of Models/Credentials).
	reader, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		log.Error(err, "unable to build client")
		os.Exit(1)
	}

	// Watches use a cache with an Agent informer only.
	ca, err := cache.New(cfg, cache.Options{Scheme: scheme})
	if err != nil {
		log.Error(err, "unable to build cache")
		os.Exit(1)
	}

	s := &server{reader: reader, hub: newHub(), log: log}

	informer, err := ca.GetInformer(ctx, &corev1alpha1.Agent{})
	if err != nil {
		log.Error(err, "unable to get Agent informer")
		os.Exit(1)
	}
	if _, err := informer.AddEventHandler(toolscache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj interface{}) { s.onAgentChange(ctx, obj) },
		UpdateFunc: func(_, obj interface{}) { s.onAgentChange(ctx, obj) },
	}); err != nil {
		log.Error(err, "unable to register Agent event handler")
		os.Exit(1)
	}

	go func() {
		if err := ca.Start(ctx); err != nil {
			log.Error(err, "cache stopped")
		}
	}()
	if !ca.WaitForCacheSync(ctx) {
		log.Error(fmt.Errorf("cache did not sync"), "startup failed")
		os.Exit(1)
	}

	mux := http.NewServeMux()
	// GET /v1/agents/{namespace}/{name}/config  -> one-shot snapshot
	// GET /v1/agents/{namespace}/{name}/watch   -> SSE stream of updates
	mux.HandleFunc("/v1/agents/", s.handleAgent)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })

	log.Info("registry listening", "addr", addr)
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Error(err, "server exited")
		os.Exit(1)
	}
}

// server serves both the snapshot and streaming endpoints.
type server struct {
	reader client.Reader
	hub    *hub
	log    logr
}

// logr is the minimal logging surface used here (avoids importing the full
// logr types at call sites).
type logr = interface {
	Info(msg string, keysAndValues ...any)
	Error(err error, msg string, keysAndValues ...any)
}

// handleAgent dispatches /v1/agents/{ns}/{name}/{config|watch}.
func (s *server) handleAgent(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/v1/agents/"), "/")
	if len(parts) != 3 {
		http.Error(w, "expected /v1/agents/{namespace}/{name}/{config|watch}", http.StatusBadRequest)
		return
	}
	ns, name, verb := parts[0], parts[1], parts[2]
	switch verb {
	case "config":
		s.handleConfig(w, r, ns, name)
	case "watch":
		s.handleWatch(w, r, ns, name)
	default:
		http.Error(w, "unknown verb: "+verb, http.StatusBadRequest)
	}
}

// handleConfig returns a one-shot snapshot of the resolved config.
func (s *server) handleConfig(w http.ResponseWriter, r *http.Request, ns, name string) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	cfg, err := s.buildConfig(ctx, ns, name)
	if err != nil {
		http.Error(w, fmt.Sprintf("agent %s/%s: %v", ns, name, err), http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(cfg); err != nil {
		s.log.Error(err, "encode config failed")
	}
}

// handleWatch streams config updates for one Agent via Server-Sent Events. It
// sends the current snapshot immediately, then a new event whenever the Agent
// changes, plus periodic keepalives so idle connections stay open.
func (s *server) handleWatch(w http.ResponseWriter, r *http.Request, ns, name string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	key := ns + "/" + name

	// Initial snapshot (best-effort — the agent may not exist yet).
	if cfg, err := s.buildConfig(r.Context(), ns, name); err == nil {
		writeSSE(w, cfg)
		flusher.Flush()
	}

	sub := s.hub.subscribe(key)
	defer s.hub.unsubscribe(key, sub)

	keepalive := time.NewTicker(25 * time.Second)
	defer keepalive.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case cfg := <-sub.ch:
			writeSSE(w, cfg)
			flusher.Flush()
		case <-keepalive.C:
			_, _ = io.WriteString(w, ": keepalive\n\n")
			flusher.Flush()
		}
	}
}

// onAgentChange is invoked by the informer; it rebuilds and broadcasts config.
func (s *server) onAgentChange(ctx context.Context, obj interface{}) {
	agent, ok := obj.(*corev1alpha1.Agent)
	if !ok {
		return
	}
	cfg, err := s.buildConfigFrom(ctx, agent)
	if err != nil {
		s.log.Error(err, "build config on change failed", "agent", agent.Namespace+"/"+agent.Name)
		return
	}
	s.hub.broadcast(agent.Namespace+"/"+agent.Name, cfg)
}

// buildConfig loads the Agent by name and resolves its config.
func (s *server) buildConfig(ctx context.Context, ns, name string) (sdk.AgentConfig, error) {
	var agent corev1alpha1.Agent
	if err := s.reader.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &agent); err != nil {
		return sdk.AgentConfig{}, err
	}
	return s.buildConfigFrom(ctx, &agent)
}

// buildConfigFrom resolves the config for an already-loaded Agent, enriching it
// with the referenced Model so runtimes avoid a second round-trip.
func (s *server) buildConfigFrom(ctx context.Context, agent *corev1alpha1.Agent) (sdk.AgentConfig, error) {
	// Apply AgentClass defaults so the shipped spec is the *effective* one — the
	// runtime sees the same defaulted refs the Operator resolved and hashed.
	var class *corev1alpha1.AgentClass
	if agent.Spec.AgentClassRef != nil {
		var c corev1alpha1.AgentClass
		if err := s.reader.Get(ctx, client.ObjectKey{Namespace: agent.Namespace, Name: agent.Spec.AgentClassRef.Name}, &c); err == nil {
			class = &c
		}
	}
	eff := corev1alpha1.ApplyClassDefaults(agent.Spec, class)

	// Ship the effective spec as raw JSON for runtime introspection; everything
	// actionable is resolved into the typed views below.
	specJSON, err := json.Marshal(eff)
	if err != nil {
		return sdk.AgentConfig{}, fmt.Errorf("marshal effective spec: %w", err)
	}

	out := sdk.AgentConfig{
		Namespace:  agent.Namespace,
		Name:       agent.Name,
		ConfigHash: agent.Status.ResolvedConfigHash,
		Phase:      string(agent.Status.Phase),
		Spec:       specJSON,
	}
	// Resolve the system prompt server-side so runtimes need no access to
	// PromptTemplate CRs (they keep RBAC for Secrets only).
	if eff.PromptRef != nil {
		var pt corev1alpha1.PromptTemplate
		if err := s.reader.Get(ctx, client.ObjectKey{Namespace: agent.Namespace, Name: eff.PromptRef.Name}, &pt); err == nil {
			out.Prompt = &sdk.Prompt{Name: pt.Name, System: pt.Spec.System}
		} else {
			s.log.Error(err, "resolve promptTemplate", "promptTemplate", eff.PromptRef.Name)
		}
	}
	// Resolve the Workflow step graph the same way. Agent Plane never executes
	// it — the runtime interprets the engine-neutral graph.
	if eff.WorkflowRef != nil {
		var wf corev1alpha1.Workflow
		if err := s.reader.Get(ctx, client.ObjectKey{Namespace: agent.Namespace, Name: eff.WorkflowRef.Name}, &wf); err == nil {
			wv := &sdk.Workflow{Name: wf.Name, Engine: wf.Spec.Engine, Version: wf.Spec.Version}
			for _, st := range wf.Spec.Steps {
				wv.Steps = append(wv.Steps, sdk.WorkflowStep{Name: st.Name, Type: st.Type, Next: st.Next})
			}
			out.Workflow = wv
		} else {
			s.log.Error(err, "resolve workflow", "workflow", eff.WorkflowRef.Name)
		}
	}
	if eff.ModelRef != nil {
		var model corev1alpha1.Model
		if err := s.reader.Get(ctx, client.ObjectKey{Namespace: agent.Namespace, Name: eff.ModelRef.Name}, &model); err == nil {
			mv := &sdk.Model{
				Provider:  string(model.Spec.Provider),
				ModelName: model.Spec.ModelName,
				Endpoint:  model.Spec.Endpoint,
			}
			// Resolve the credential to its Secret coordinates (not the value).
			if model.Spec.CredentialRef != nil {
				var cred corev1alpha1.Credential
				if err := s.reader.Get(ctx, client.ObjectKey{Namespace: agent.Namespace, Name: model.Spec.CredentialRef.Name}, &cred); err == nil {
					mv.SecretName = cred.Spec.SecretRef.Name
					mv.SecretKey = cred.Spec.SecretRef.Key
				}
			}
			out.Model = mv
		}
	}
	// Resolve tools from direct toolRefs AND expand toolSetRefs into their
	// member tools, deduping by name so a tool referenced both ways appears once.
	seenTool := map[string]bool{}
	addTool := func(name string) {
		if seenTool[name] {
			return
		}
		tv, err := s.resolveTool(ctx, agent.Namespace, name)
		if err != nil {
			s.log.Error(err, "resolve tool", "tool", name)
			return
		}
		seenTool[name] = true
		out.Tools = append(out.Tools, tv)
	}
	for _, ref := range eff.ToolRefs {
		addTool(ref.Name)
	}
	for _, ref := range eff.ToolSetRefs {
		var ts corev1alpha1.ToolSet
		if err := s.reader.Get(ctx, client.ObjectKey{Namespace: agent.Namespace, Name: ref.Name}, &ts); err != nil {
			s.log.Error(err, "resolve toolset", "toolset", ref.Name)
			continue
		}
		for _, t := range ts.Spec.ToolRefs {
			addTool(t.Name)
		}
	}
	for _, ref := range eff.SkillRefs {
		sv, err := s.resolveSkill(ctx, agent.Namespace, ref.Name)
		if err != nil {
			s.log.Error(err, "resolve skill", "skill", ref.Name)
			continue
		}
		out.Skills = append(out.Skills, sv)
	}
	for _, ref := range eff.MemoryRefs {
		mv, err := s.resolveMemory(ctx, agent.Namespace, ref.Name)
		if err != nil {
			s.log.Error(err, "resolve memory", "memory", ref.Name)
			continue
		}
		out.Memories = append(out.Memories, mv)
	}
	for _, ref := range eff.KnowledgeBaseRefs {
		kv, err := s.resolveKnowledgeBase(ctx, agent.Namespace, ref.Name)
		if err != nil {
			s.log.Error(err, "resolve knowledgeBase", "knowledgeBase", ref.Name)
			continue
		}
		out.Knowledge = append(out.Knowledge, kv)
	}
	out.Policy = s.resolvePolicy(ctx, agent.Namespace, eff)
	return out, nil
}

// resolvePolicy merges the Agent's Policies and ToolPolicies into the view the
// runtime enforces. It uses the same MergePolicies the Operator uses to decide
// whether the Agent may run at all, so the two halves of enforcement cannot
// disagree about what a set of policies means. Returns nil when nothing is
// constrained.
func (s *server) resolvePolicy(ctx context.Context, ns string, eff corev1alpha1.AgentSpec) *sdk.Policy {
	policies := make([]corev1alpha1.Policy, 0, len(eff.PolicyRefs))
	toolPolicies := make([]corev1alpha1.ToolPolicy, 0, len(eff.ToolPolicyRefs))
	for _, ref := range eff.PolicyRefs {
		var p corev1alpha1.Policy
		if err := s.reader.Get(ctx, client.ObjectKey{Namespace: ns, Name: ref.Name}, &p); err != nil {
			// A missing Policy already keeps the Agent out of Ready, so a runtime
			// will not act on this config; log and carry on rather than failing the
			// whole snapshot.
			s.log.Error(err, "resolve policy", "policy", ref.Name)
			continue
		}
		policies = append(policies, p)
	}
	for _, ref := range eff.ToolPolicyRefs {
		var tp corev1alpha1.ToolPolicy
		if err := s.reader.Get(ctx, client.ObjectKey{Namespace: ns, Name: ref.Name}, &tp); err != nil {
			s.log.Error(err, "resolve toolPolicy", "toolPolicy", ref.Name)
			continue
		}
		toolPolicies = append(toolPolicies, tp)
	}
	merged := corev1alpha1.MergePolicies(policies, toolPolicies)
	if merged == nil {
		return nil
	}
	pv := &sdk.Policy{
		Sources:           merged.Sources,
		Models:            accessRuleView(merged.Models),
		Memory:            accessRuleView(merged.Memory),
		MCP:               accessRuleView(merged.MCP),
		Tools:             accessRuleView(merged.Tools),
		Workflows:         accessRuleView(merged.Workflows),
		DefaultToolAction: string(merged.DefaultToolAction),
	}
	for _, r := range merged.ToolRules {
		pv.ToolRules = append(pv.ToolRules, sdk.ToolRule{
			Tool:               r.Tool,
			Action:             string(r.Action),
			MaxCallsPerSession: r.MaxCallsPerSession,
		})
	}
	return pv
}

func accessRuleView(r *corev1alpha1.AccessRule) *sdk.AccessRule {
	if r == nil {
		return nil
	}
	return &sdk.AccessRule{Allow: r.Allow, Deny: r.Deny}
}

// resolveTool turns a Tool reference into a fully-resolved definition. For mcp
// tools it resolves the backing MCPServer's in-cluster endpoint from status.
func (s *server) resolveTool(ctx context.Context, ns, name string) (sdk.Tool, error) {
	var tool corev1alpha1.Tool
	if err := s.reader.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &tool); err != nil {
		return sdk.Tool{}, err
	}
	tv := sdk.Tool{
		Name:        tool.Name,
		Type:        string(tool.Spec.Type),
		Description: tool.Spec.Description,
		Endpoint:    tool.Spec.Endpoint,
		MCPToolName: tool.Spec.MCPToolName,
	}
	if tool.Spec.InputSchema != nil {
		tv.InputSchema = tool.Spec.InputSchema.Raw
	}
	if tool.Spec.Type == "mcp" && tool.Spec.MCPServerRef != nil {
		var mcp corev1alpha1.MCPServer
		if err := s.reader.Get(ctx, client.ObjectKey{Namespace: ns, Name: tool.Spec.MCPServerRef.Name}, &mcp); err == nil {
			tv.Endpoint = mcp.Status.Endpoint
		}
	}
	return tv, nil
}

// resolveSkill loads a Skill and returns its instruction content. Content comes
// from the inline spec.content or, failing that, the referenced ConfigMap.
func (s *server) resolveSkill(ctx context.Context, ns, name string) (sdk.Skill, error) {
	var skill corev1alpha1.Skill
	if err := s.reader.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &skill); err != nil {
		return sdk.Skill{}, err
	}
	sv := sdk.Skill{Name: skill.Name, Description: skill.Spec.Description, Content: skill.Spec.Content}
	if sv.Content == "" && skill.Spec.ContentConfigMapRef != nil {
		ref := skill.Spec.ContentConfigMapRef
		var cm corev1.ConfigMap
		if err := s.reader.Get(ctx, client.ObjectKey{Namespace: ns, Name: ref.Name}, &cm); err == nil {
			sv.Content = cm.Data[ref.Key]
		}
	}
	return sv, nil
}

// resolveMemory loads a Memory and returns its backend + the Secret coordinates
// (never the value) of its connection Credential.
func (s *server) resolveMemory(ctx context.Context, ns, name string) (sdk.Memory, error) {
	var mem corev1alpha1.Memory
	if err := s.reader.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &mem); err != nil {
		return sdk.Memory{}, err
	}
	mv := sdk.Memory{Name: mem.Name, Backend: string(mem.Spec.Backend), Namespace: mem.Spec.Namespace}
	if mem.Spec.ConnectionRef != nil {
		var cred corev1alpha1.Credential
		if err := s.reader.Get(ctx, client.ObjectKey{Namespace: ns, Name: mem.Spec.ConnectionRef.Name}, &cred); err == nil {
			mv.SecretName = cred.Spec.SecretRef.Name
			mv.SecretKey = cred.Spec.SecretRef.Key
		}
	}
	return mv, nil
}

// resolveKnowledgeBase loads a KnowledgeBase and returns its source coordinates,
// resolving the embedding Model name and access-credential Secret coordinates.
func (s *server) resolveKnowledgeBase(ctx context.Context, ns, name string) (sdk.KnowledgeBase, error) {
	var kb corev1alpha1.KnowledgeBase
	if err := s.reader.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &kb); err != nil {
		return sdk.KnowledgeBase{}, err
	}
	kv := sdk.KnowledgeBase{Name: kb.Name, Source: string(kb.Spec.Source), URI: kb.Spec.URI}
	if kb.Spec.EmbeddingModelRef != nil {
		var model corev1alpha1.Model
		if err := s.reader.Get(ctx, client.ObjectKey{Namespace: ns, Name: kb.Spec.EmbeddingModelRef.Name}, &model); err == nil {
			kv.EmbeddingModel = model.Spec.ModelName
		}
	}
	if kb.Spec.CredentialRef != nil {
		var cred corev1alpha1.Credential
		if err := s.reader.Get(ctx, client.ObjectKey{Namespace: ns, Name: kb.Spec.CredentialRef.Name}, &cred); err == nil {
			kv.SecretName = cred.Spec.SecretRef.Name
			kv.SecretKey = cred.Spec.SecretRef.Key
		}
	}
	return kv, nil
}

func writeSSE(w io.Writer, cfg sdk.AgentConfig) {
	b, err := json.Marshal(cfg)
	if err != nil {
		return
	}
	_, _ = fmt.Fprintf(w, "data: %s\n\n", b)
}

// hub fans out per-agent config updates to SSE subscribers.
type hub struct {
	mu   sync.Mutex
	subs map[string]map[*subscriber]struct{}
}

type subscriber struct {
	ch chan sdk.AgentConfig
}

func newHub() *hub {
	return &hub{subs: make(map[string]map[*subscriber]struct{})}
}

func (h *hub) subscribe(key string) *subscriber {
	sub := &subscriber{ch: make(chan sdk.AgentConfig, 8)}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.subs[key] == nil {
		h.subs[key] = make(map[*subscriber]struct{})
	}
	h.subs[key][sub] = struct{}{}
	return sub
}

func (h *hub) unsubscribe(key string, sub *subscriber) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if set, ok := h.subs[key]; ok {
		delete(set, sub)
		if len(set) == 0 {
			delete(h.subs, key)
		}
	}
	close(sub.ch)
}

// broadcast delivers cfg to every subscriber of key. Sends are non-blocking:
// a slow consumer drops intermediate updates but always converges, since each
// event carries the full current config (not a delta).
func (h *hub) broadcast(key string, cfg sdk.AgentConfig) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for sub := range h.subs[key] {
		select {
		case sub.ch <- cfg:
		default:
		}
	}
}
