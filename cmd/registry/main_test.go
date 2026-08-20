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

package main

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	sdk "github.com/hkmdxlftjf/agent-plane-sdk-go"

	corev1alpha1 "github.com/hkmdxlftjf/agent-plane/api/v1alpha1"
)

// discardLog satisfies the server's minimal logging surface.
type discardLog struct{}

func (discardLog) Info(string, ...any)         {}
func (discardLog) Error(error, string, ...any) {}

func testServer(t *testing.T, objs ...runtime.Object) *server {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("add client-go scheme: %v", err)
	}
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add agent-plane scheme: %v", err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(objs...).Build()
	return &server{reader: c, log: discardLog{}}
}

const (
	testNS     = "default"
	toolRefund = "refund"
)

// With no policy referenced the served view must be nil, so a runtime can tell
// "unconstrained" from "constrained by an empty policy".
func TestResolvePolicyNone(t *testing.T) {
	s := testServer(t)
	got := s.resolvePolicy(context.Background(), testNS, corev1alpha1.AgentSpec{})
	if got != nil {
		t.Errorf("resolvePolicy = %+v, want nil", got)
	}
}

// The Registry must ship the merged policy so the runtime can enforce the
// call-time half. Before this, cfg.Policy was always absent and a ToolPolicy
// had no effect anywhere.
func TestResolvePolicyServesMergedView(t *testing.T) {
	cap2 := int32(2)
	s := testServer(t,
		&corev1alpha1.Policy{
			ObjectMeta: metav1.ObjectMeta{Name: "guardrails", Namespace: testNS},
			Spec: corev1alpha1.PolicySpec{
				Models: &corev1alpha1.AccessRule{Allow: []string{"claude"}},
				Tools:  &corev1alpha1.AccessRule{Deny: []string{"delete-account"}},
			},
		},
		&corev1alpha1.ToolPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: "tool-limits", Namespace: testNS},
			Spec: corev1alpha1.ToolPolicySpec{
				Rules: []corev1alpha1.ToolRule{
					{Tool: toolRefund, Action: corev1alpha1.ToolActionAllow, MaxCallsPerSession: &cap2},
					{Tool: "*", Action: corev1alpha1.ToolActionDeny},
				},
				DefaultAction: corev1alpha1.ToolActionDeny,
			},
		},
	)

	got := s.resolvePolicy(context.Background(), testNS, corev1alpha1.AgentSpec{
		PolicyRefs:     []corev1alpha1.LocalReference{{Name: "guardrails"}},
		ToolPolicyRefs: []corev1alpha1.LocalReference{{Name: "tool-limits"}},
	})
	if got == nil {
		t.Fatal("resolvePolicy = nil, want a merged view")
	}
	if strings.Join(got.Sources, ",") != "Policy/guardrails,ToolPolicy/tool-limits" {
		t.Errorf("Sources = %v", got.Sources)
	}
	if got.Models == nil || len(got.Models.Allow) != 1 || got.Models.Allow[0] != "claude" {
		t.Errorf("Models = %+v, want allow=[claude]", got.Models)
	}
	if got.Tools == nil || len(got.Tools.Deny) != 1 || got.Tools.Deny[0] != "delete-account" {
		t.Errorf("Tools = %+v, want deny=[delete-account]", got.Tools)
	}
	if got.DefaultToolAction != string(corev1alpha1.ToolActionDeny) {
		t.Errorf("DefaultToolAction = %q, want deny", got.DefaultToolAction)
	}
	if len(got.ToolRules) != 2 {
		t.Fatalf("ToolRules = %+v, want 2", got.ToolRules)
	}
	// The cap must survive the CR -> wire conversion; it is the whole point of
	// shipping the view, since only the runtime can count calls.
	if got.ToolRules[0].MaxCallsPerSession == nil || *got.ToolRules[0].MaxCallsPerSession != 2 {
		t.Errorf("MaxCallsPerSession = %v, want 2", got.ToolRules[0].MaxCallsPerSession)
	}
	if got.ToolRules[0].Action != string(corev1alpha1.ToolActionAllow) {
		t.Errorf("rule action = %q, want allow", got.ToolRules[0].Action)
	}
}

// A missing Policy already keeps the Agent out of Ready, so the snapshot must
// still build rather than failing and taking the whole config down with it.
func TestResolvePolicyToleratesMissingRefs(t *testing.T) {
	s := testServer(t, &corev1alpha1.Policy{
		ObjectMeta: metav1.ObjectMeta{Name: "present", Namespace: testNS},
		Spec: corev1alpha1.PolicySpec{
			Models: &corev1alpha1.AccessRule{Deny: []string{"gpt-4"}},
		},
	})

	got := s.resolvePolicy(context.Background(), testNS, corev1alpha1.AgentSpec{
		PolicyRefs: []corev1alpha1.LocalReference{{Name: "present"}, {Name: "absent"}},
	})
	if got == nil {
		t.Fatal("resolvePolicy = nil, want the resolvable half")
	}
	if len(got.Sources) != 1 || got.Sources[0] != "Policy/present" {
		t.Errorf("Sources = %v, want only the Policy that exists", got.Sources)
	}
}

// buildConfigFrom must attach the policy view, since that is the field the
// runtime actually reads.
func TestBuildConfigFromAttachesPolicy(t *testing.T) {
	s := testServer(t,
		&corev1alpha1.Model{
			ObjectMeta: metav1.ObjectMeta{Name: "m", Namespace: testNS},
			Spec:       corev1alpha1.ModelSpec{Provider: "anthropic", ModelName: "claude-opus-4-8"},
		},
		&corev1alpha1.ToolPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: "tp", Namespace: testNS},
			Spec: corev1alpha1.ToolPolicySpec{
				Rules: []corev1alpha1.ToolRule{{Tool: toolRefund, Action: corev1alpha1.ToolActionDeny}},
			},
		},
	)
	agent := &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: testNS},
		Spec: corev1alpha1.AgentSpec{
			ModelRef:       &corev1alpha1.LocalReference{Name: "m"},
			ToolPolicyRefs: []corev1alpha1.LocalReference{{Name: "tp"}},
		},
	}

	cfg, err := s.buildConfigFrom(context.Background(), agent)
	if err != nil {
		t.Fatalf("buildConfigFrom: %v", err)
	}
	if cfg.Policy == nil {
		t.Fatal("cfg.Policy = nil; the runtime would enforce nothing")
	}
	if len(cfg.Policy.ToolRules) != 1 || cfg.Policy.ToolRules[0].Tool != toolRefund {
		t.Errorf("ToolRules = %+v", cfg.Policy.ToolRules)
	}
}

// AgentClass defaults must flow into policy resolution too, otherwise a class
// that attaches guardrails to every Agent of its kind would be ignored on the
// data-plane side while the Operator still enforced it.
func TestResolvePolicyHonorsAgentClassDefaults(t *testing.T) {
	s := testServer(t,
		&corev1alpha1.Model{
			ObjectMeta: metav1.ObjectMeta{Name: "m", Namespace: testNS},
			Spec:       corev1alpha1.ModelSpec{Provider: "anthropic", ModelName: "claude-opus-4-8"},
		},
		&corev1alpha1.AgentClass{
			ObjectMeta: metav1.ObjectMeta{Name: "class", Namespace: testNS},
			Spec: corev1alpha1.AgentClassSpec{
				DefaultPolicyRefs:     []corev1alpha1.LocalReference{{Name: "class-policy"}},
				DefaultToolPolicyRefs: []corev1alpha1.LocalReference{{Name: "class-toolpolicy"}},
			},
		},
		&corev1alpha1.Policy{
			ObjectMeta: metav1.ObjectMeta{Name: "class-policy", Namespace: testNS},
			Spec: corev1alpha1.PolicySpec{
				Models: &corev1alpha1.AccessRule{Deny: []string{"gpt-4"}},
			},
		},
		&corev1alpha1.ToolPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: "class-toolpolicy", Namespace: testNS},
			Spec: corev1alpha1.ToolPolicySpec{
				DefaultAction: corev1alpha1.ToolActionDeny,
			},
		},
	)
	agent := &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: testNS},
		Spec: corev1alpha1.AgentSpec{
			AgentClassRef: &corev1alpha1.LocalReference{Name: "class"},
			ModelRef:      &corev1alpha1.LocalReference{Name: "m"},
		},
	}

	cfg, err := s.buildConfigFrom(context.Background(), agent)
	if err != nil {
		t.Fatalf("buildConfigFrom: %v", err)
	}
	if cfg.Policy == nil {
		t.Fatal("cfg.Policy = nil; the class defaults were dropped")
	}
	if cfg.Policy.Models == nil || len(cfg.Policy.Models.Deny) != 1 {
		t.Errorf("Models = %+v, want the class Policy's deny", cfg.Policy.Models)
	}
	if cfg.Policy.DefaultToolAction != string(corev1alpha1.ToolActionDeny) {
		t.Errorf("DefaultToolAction = %q, want the class ToolPolicy's deny", cfg.Policy.DefaultToolAction)
	}
}

// The Registry must ship allowedTools: without it the runtime cannot confine
// tool calls after a skill loads, and the field would be inert again.
func TestResolveSkillServesAllowedTools(t *testing.T) {
	s := testServer(t, &corev1alpha1.Skill{
		ObjectMeta: metav1.ObjectMeta{Name: "refunds", Namespace: testNS},
		Spec: corev1alpha1.SkillSpec{
			Description:  "process a refund",
			Content:      "STEP 1 …",
			AllowedTools: []string{toolRefund, "order-lookup"},
		},
	})

	got, err := s.resolveSkill(context.Background(), testNS, "refunds")
	if err != nil {
		t.Fatalf("resolveSkill: %v", err)
	}
	if strings.Join(got.AllowedTools, ",") != toolRefund+",order-lookup" {
		t.Errorf("AllowedTools = %v", got.AllowedTools)
	}
	if got.Content != "STEP 1 …" {
		t.Errorf("Content = %q", got.Content)
	}
}

// A skill that restricts nothing must serve an empty list, not a phantom
// restriction.
func TestResolveSkillWithoutAllowedTools(t *testing.T) {
	s := testServer(t, &corev1alpha1.Skill{
		ObjectMeta: metav1.ObjectMeta{Name: "prose", Namespace: testNS},
		Spec:       corev1alpha1.SkillSpec{Description: "advice", Content: "just prose"},
	})

	got, err := s.resolveSkill(context.Background(), testNS, "prose")
	if err != nil {
		t.Fatalf("resolveSkill: %v", err)
	}
	if len(got.AllowedTools) != 0 {
		t.Errorf("AllowedTools = %v, want empty", got.AllowedTools)
	}
}

// A ConfigMap-sourced body must still carry allowedTools — the restriction lives
// on the Skill, not in the body.
func TestResolveSkillFromConfigMapKeepsAllowedTools(t *testing.T) {
	s := testServer(t,
		&corev1alpha1.Skill{
			ObjectMeta: metav1.ObjectMeta{Name: "big", Namespace: testNS},
			Spec: corev1alpha1.SkillSpec{
				Description: "a large skill",
				ContentConfigMapRef: &corev1alpha1.ConfigMapKeyReference{
					Name: "skill-cm", Key: "SKILL.md",
				},
				AllowedTools: []string{toolRefund},
			},
		},
		&corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: "skill-cm", Namespace: testNS},
			Data:       map[string]string{"SKILL.md": "# from configmap"},
		},
	)

	got, err := s.resolveSkill(context.Background(), testNS, "big")
	if err != nil {
		t.Fatalf("resolveSkill: %v", err)
	}
	if got.Content != "# from configmap" {
		t.Errorf("Content = %q, want the ConfigMap body", got.Content)
	}
	if len(got.AllowedTools) != 1 || got.AllowedTools[0] != toolRefund {
		t.Errorf("AllowedTools = %v", got.AllowedTools)
	}
}

// A peer Tool must resolve to the target Agent's published endpoint, so
// consulting another Agent looks to the runtime exactly like any other MCP
// server — that equivalence is what lets Policy govern cross-repo calls.
func TestResolveToolResolvesPeerAgent(t *testing.T) {
	s := testServer(t,
		&corev1alpha1.Agent{
			ObjectMeta: metav1.ObjectMeta{Name: "web-frontend", Namespace: testNS},
			Spec: corev1alpha1.AgentSpec{
				Expose: &corev1alpha1.AgentExposeSpec{Description: "Owns the web frontend"},
			},
			Status: corev1alpha1.AgentStatus{
				PeerEndpoint: "http://web-frontend-peer.default.svc:8080",
			},
		},
		&corev1alpha1.Tool{
			ObjectMeta: metav1.ObjectMeta{Name: "ask-web", Namespace: testNS},
			Spec: corev1alpha1.ToolSpec{
				Type:     corev1alpha1.ToolTypeMCP,
				AgentRef: &corev1alpha1.LocalReference{Name: "web-frontend"},
			},
		},
	)

	got, err := s.resolveTool(context.Background(), testNS, "ask-web")
	if err != nil {
		t.Fatalf("resolveTool: %v", err)
	}
	if got.Endpoint != "http://web-frontend-peer.default.svc:8080" {
		t.Errorf("Endpoint = %q, want the peer endpoint", got.Endpoint)
	}
	// With no description of its own, the Tool inherits the peer's — that text is
	// what tells the calling model when asking is worthwhile.
	if got.Description != "Owns the web frontend" {
		t.Errorf("Description = %q, want the peer's", got.Description)
	}
}

// A Tool's own description wins over the peer's, so an operator can phrase the
// capability from the caller's point of view.
func TestResolveToolPeerDescriptionOverride(t *testing.T) {
	s := testServer(t,
		&corev1alpha1.Agent{
			ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: testNS},
			Spec:       corev1alpha1.AgentSpec{Expose: &corev1alpha1.AgentExposeSpec{Description: "generic"}},
			Status:     corev1alpha1.AgentStatus{PeerEndpoint: "http://web-peer.default.svc:8080"},
		},
		&corev1alpha1.Tool{
			ObjectMeta: metav1.ObjectMeta{Name: "ask-web", Namespace: testNS},
			Spec: corev1alpha1.ToolSpec{
				Type:        corev1alpha1.ToolTypeMCP,
				Description: "Ask the frontend team about component APIs",
				AgentRef:    &corev1alpha1.LocalReference{Name: "web"},
			},
		},
	)

	got, err := s.resolveTool(context.Background(), testNS, "ask-web")
	if err != nil {
		t.Fatalf("resolveTool: %v", err)
	}
	if got.Description != "Ask the frontend team about component APIs" {
		t.Errorf("Description = %q, want the Tool's own", got.Description)
	}
}

// A peer that has not published an endpoint yet must leave it empty rather than
// inventing an address; the caller's Agent is already Degraded in that state.
func TestResolveToolPeerWithoutEndpoint(t *testing.T) {
	s := testServer(t,
		&corev1alpha1.Agent{
			ObjectMeta: metav1.ObjectMeta{Name: "not-ready", Namespace: testNS},
			Spec:       corev1alpha1.AgentSpec{Expose: &corev1alpha1.AgentExposeSpec{}},
		},
		&corev1alpha1.Tool{
			ObjectMeta: metav1.ObjectMeta{Name: "ask-not-ready", Namespace: testNS},
			Spec: corev1alpha1.ToolSpec{
				Type:     corev1alpha1.ToolTypeMCP,
				AgentRef: &corev1alpha1.LocalReference{Name: "not-ready"},
			},
		},
	)

	got, err := s.resolveTool(context.Background(), testNS, "ask-not-ready")
	if err != nil {
		t.Fatalf("resolveTool: %v", err)
	}
	if got.Endpoint != "" {
		t.Errorf("Endpoint = %q, want empty", got.Endpoint)
	}
}

// waitForSubscriber polls until the hub has at least one subscriber for key.
func waitForSubscriber(t *testing.T, h *hub, key string) {
	t.Helper()
	for i := 0; i < 1000; i++ {
		if h.subscriberCount(key) > 0 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("no subscriber registered in time")
}

// A /watch client must receive exactly the snapshot plus newer frames:
// a duplicate of the snapshot's resourceVersion and a regressed (older-cache)
// frame are both dropped, so a client can never roll back to older config.
func TestWatchDropsDuplicateAndRegressionFrames(t *testing.T) {
	agent := &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: "watched", ResourceVersion: "5"},
	}
	s := testServer(t, agent)
	s.hub = newHub()

	rec := httptest.NewRecorder()
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest("GET", "/v1/agents/"+testNS+"/watched/watch", nil).WithContext(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.handleWatch(rec, req, testNS, "watched")
	}()

	key := testNS + "/watched"
	waitForSubscriber(t, s.hub, key)

	// Informer replay of the snapshot object, a stale-cache frame, then a real update.
	s.hub.broadcast(key, "5", sdk.AgentConfig{ConfigHash: "dup"})
	s.hub.broadcast(key, "4", sdk.AgentConfig{ConfigHash: "stale"})
	s.hub.broadcast(key, "6", sdk.AgentConfig{ConfigHash: "fresh"})

	time.Sleep(50 * time.Millisecond)
	cancel()
	<-done

	body := rec.Body.String()
	if n := strings.Count(body, `"configHash":"fresh"`); n != 1 {
		t.Errorf("newer frame must be delivered exactly once, got %d in body: %s", n, body)
	}
	if strings.Contains(body, `"configHash":"stale"`) {
		t.Error("a frame older than the snapshot must be dropped, not delivered")
	}
	if n := strings.Count(body, `"configHash":"dup"`); n != 0 {
		t.Errorf("duplicate of the snapshot frame must be dropped, got %d in body: %s", n, body)
	}
	if !strings.Contains(body, `"configHash":""`) && !strings.Contains(body, "data:") {
		t.Error("initial snapshot must be sent")
	}
}
