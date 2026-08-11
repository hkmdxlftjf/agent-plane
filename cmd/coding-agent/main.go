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

// Command coding-agent is a real agent runtime for repository-bound Agents
// (spec.workspace): a model-driven edit loop that reads and writes files in
// the cloned working tree and runs shell commands (git, build tools, test
// runners) against it, built on the same runtime SDK as cmd/agent-runtime
// (github.com/hkmdxlftjf/agent-plane-sdk-go).
//
// It differs from cmd/agent-runtime in exactly the ways a coding agent needs
// to: it adds four in-process tools (bash, read_file, write_file,
// request_confirmation) and appends a fixed system-prompt policy telling the
// model to stop and call request_confirmation before destructive or uncertain
// actions rather than act on a guess. request_confirmation does not send
// anything itself — it records a pending confirmation on the session, which
// /api/chat's response then carries alongside the answer (see the
// confirmation field), so any adapter (Lark, DingTalk, …) can render it its
// own way (an interactive card, a plain-text prompt, …) without this runtime
// knowing which platform it is talking to. Everything else (config pull,
// secrets, memory, retrieval, policy enforcement) is the same reference
// wiring as cmd/agent-runtime.
//
// File isolation has two layers: the Operator confines the *container* (a
// read-only root filesystem, no capabilities, non-root — see
// internal/controller/agent_controller.go) so nothing outside the working
// tree and /tmp is writable regardless of what runs inside it; read_file and
// write_file additionally confine *paths* to the working tree as a second,
// independent check. bash is intentionally not path-confined — it is exactly
// as powerful as a shell in this container, which the container-level sandbox
// bounds instead.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	sdk "github.com/hkmdxlftjf/agent-plane-sdk-go"
	"github.com/hkmdxlftjf/agent-plane-sdk-go/agentloop"
	"github.com/hkmdxlftjf/agent-plane-sdk-go/memory"
	"github.com/hkmdxlftjf/agent-plane-sdk-go/policy"
	"github.com/hkmdxlftjf/agent-plane-sdk-go/retriever"
	"github.com/hkmdxlftjf/agent-plane-sdk-go/secrets"
)

func main() {
	var registry, ns, name, workspace, prompt string
	var maxSteps int
	var bashTimeout time.Duration
	var watch, chatMode, serveMode bool
	var serveAddr string
	flag.StringVar(&registry, "registry", envOr("AGENTPLANE_REGISTRY", "http://localhost:9090"), "Registry base URL")
	flag.StringVar(&ns, "namespace", envOr("AGENTPLANE_AGENT_NAMESPACE", "default"), "Agent namespace")
	flag.StringVar(&name, "name", envOr("AGENTPLANE_AGENT_NAME", "coding-agent"), "Agent name")
	flag.StringVar(&workspace, "workspace", envOr("AGENTPLANE_WORKSPACE", "/workspace"), "working tree root")
	flag.StringVar(&prompt, "prompt", "", "one-shot instruction (non-interactive mode)")
	flag.IntVar(&maxSteps, "max-steps", 40, "max tool-calling turns per message")
	flag.DurationVar(&bashTimeout, "bash-timeout", 180*time.Second, "max duration of a single bash call")
	flag.BoolVar(&watch, "watch", os.Getenv("AGENTPLANE_WATCH") == "1", "long-running mode: subscribe to the Registry and hot-reload config")
	flag.BoolVar(&chatMode, "chat", false, "interactive multi-turn chat (REPL over stdin), for local development")
	flag.BoolVar(&serveMode, "serve", os.Getenv("AGENTPLANE_SERVE") == "1", "serve the /api/chat HTTP API (how a Trigger-driven pod runs)")
	flag.StringVar(&serveAddr, "addr", ":"+envOr("PORT", "8080"), "listen address in serve mode")
	flag.Parse()

	ctx := context.Background()
	logf := func(f string, a ...any) { fmt.Printf("  "+f+"\n", a...) }
	rc := sdk.NewClient(registry, sdk.WithLogf(logf))

	if watch && !serveMode {
		watchLoop(ctx, rc, ns, name)
		return
	}

	cfg, err := rc.FetchConfig(ctx, ns, name)
	if err != nil {
		fatal("fetch config from registry", err)
	}
	fmt.Printf("▶ Registry config for %s/%s (phase=%s)\n", cfg.Namespace, cfg.Name, cfg.Phase)
	if cfg.Phase != sdk.PhaseReady {
		fatal("agent not ready", fmt.Errorf("phase=%s", cfg.Phase))
	}
	if cfg.Model == nil {
		fatal("resolve model", fmt.Errorf("registry returned no model view"))
	}
	fmt.Printf("  model=%s/%s\n", cfg.Model.Provider, cfg.Model.ModelName)
	for _, t := range cfg.Tools {
		fmt.Printf("  tool: %s (type=%s) -> %s\n", t.Name, t.Type, t.Endpoint)
	}

	root, err := filepath.Abs(workspace)
	if err != nil {
		fatal("resolve workspace root", err)
	}
	if real, err := filepath.EvalSymlinks(root); err == nil {
		// resolveWorkspacePath assumes root is already canonical so it only has
		// to check *descendants* for a symlink escape, not root itself.
		root = real
	}
	if fi, err := os.Stat(root); err != nil || !fi.IsDir() {
		fatal("workspace root", fmt.Errorf("%q is not a directory (is spec.workspace set on this Agent?)", root))
	}
	ensureGitIdentity(root, logf)

	sec, err := secrets.NewReader(ns)
	if err != nil {
		fatal("build secret reader", err)
	}
	apiKey, err := sec.Read(ctx, cfg.Model.SecretName, cfg.Model.SecretKey)
	if err != nil {
		fatal("read model credential secret", err)
	}

	system := buildSystemPrompt(cfg, root, logf)
	store := openMemory(ctx, sec, cfg.Memories)
	rag := newRetriever(cfg.Knowledge)

	endpoint := cfg.Model.Endpoint
	if endpoint == "" && cfg.Model.Provider == "openrouter" {
		endpoint = "https://openrouter.ai/api/v1"
	}

	base := agentloop.Config{
		Endpoint: endpoint, APIKey: apiKey, Model: cfg.Model.ModelName,
		System: system, Tools: cfg.Tools, MaxSteps: maxSteps, MaxTokens: 2048,
		Logf: logf,
	}

	enf := policy.New(cfg.Policy)
	if lines := enf.Describe(); len(lines) > 0 {
		for _, line := range lines {
			fmt.Printf("  policy: %s\n", line)
		}
	} else {
		fmt.Println("  policy: none (no Policy/ToolPolicy referenced)")
	}

	if serveMode {
		serveHTTP(ctx, name, cfg, base, root, bashTimeout, serveAddr, store, rag, enf)
		return
	}
	if chatMode {
		chatREPL(ctx, name, cfg, base, root, bashTimeout, store, rag, enf)
		return
	}
	if prompt == "" {
		fatal("no instruction", fmt.Errorf("pass -prompt, -chat, or -serve"))
	}
	sessionCfg, _ := newSessionConfig(base, cfg, enf, root, bashTimeout)
	answer, err := agentloop.Run(ctx, sessionCfg, rag.Augment(ctx, prompt))
	if err != nil {
		fatal("agent loop", err)
	}
	fmt.Printf("\n✅ %s\n", answer)
}

// confirmOption is one choice offered to the user for a pending confirmation,
// e.g. {"label":"同意","value":"approve"}. value is what a later user turn should
// echo back for the model to recognize the decision; label is what a human
// sees on a button.
type confirmOption struct {
	Label string `json:"label"`
	Value string `json:"value"`
}

// confirmRequest is what request_confirmation records, and what /api/chat
// attaches to its response alongside the answer.
type confirmRequest struct {
	Summary string          `json:"summary"`
	Options []confirmOption `json:"options"`
}

// confirmSlot holds at most one pending confirmation per session. A session
// has one active turn at a time (agentloop.Session.Send is not meant to be
// called concurrently), so a single slot — set by the request_confirmation
// tool during a Send, read and cleared by the caller right after — is enough;
// it does not need to queue.
type confirmSlot struct {
	mu      sync.Mutex
	pending *confirmRequest
	// cancel stops the in-flight Send as soon as request_confirmation is
	// called. Without it, the model merely being told (in the system prompt) to
	// stop and wait is advisory only — nothing prevents it from calling more
	// tools in the same turn anyway, which is what actually happened the first
	// time this ran against a real model: it kept exploring for a dozen more
	// steps after asking for a decision. armSend sets this before every Send,
	// and set (below) calls it, which fails the loop's next model call with
	// context.Canceled; the caller (armSend's return value) turns that
	// specific error back into a normal confirmation response instead of
	// surfacing a cancellation to the user.
	cancel context.CancelFunc
}

func (s *confirmSlot) set(c confirmRequest) {
	s.mu.Lock()
	s.pending = &c
	cancel := s.cancel
	s.cancel = nil
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// takeAndClear returns the pending confirmation, if any, and clears it — a
// confirmation is reported to the caller of Send exactly once, not replayed
// on the next unrelated turn.
func (s *confirmSlot) takeAndClear() *confirmRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.pending
	s.pending = nil
	return c
}

// sendAndConfirm runs one turn and reports both the model's answer and, if
// request_confirmation was called during it, the pending confirmation — with
// the loop actually stopped at that point (see confirmSlot's cancel field),
// not just asked nicely to stop. A confirmation-triggered stop is not a
// failure: it surfaces as a normal answer (the confirmation's summary) plus a
// non-nil confirmation, exactly as if the model itself had ended the turn
// there.
func sendAndConfirm(ctx context.Context, session *agentloop.Session, slot *confirmSlot, text string) (string, *confirmRequest, error) {
	sendCtx, cancel := context.WithCancel(ctx)
	slot.arm(cancel)
	answer, err := session.Send(sendCtx, text)
	cancel()
	if err != nil {
		if c := slot.takeAndClear(); c != nil && errors.Is(err, context.Canceled) {
			return c.Summary, c, nil
		}
		return "", nil, err
	}
	return answer, slot.takeAndClear(), nil
}

// arm records the CancelFunc that set will call the moment
// request_confirmation runs. Called once per Send, before it starts.
func (s *confirmSlot) arm(cancel context.CancelFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cancel = cancel
}

// requestConfirmationTool lets the model pause a risky or uncertain action and
// hand the decision to the user instead of guessing. It sends nothing by
// itself: it only records the request on slot, which sendAndConfirm reads
// after the turn and surfaces however that surface renders a decision (an
// interactive card, a plain question, …). The turn is stopped right here —
// slot.set cancels the Send in progress — rather than merely asked (in the
// system prompt) to stop, because a model cannot be relied on to end its turn
// on request; it may call more tools first.
func requestConfirmationTool(slot *confirmSlot) agentloop.LocalTool {
	return agentloop.LocalTool{
		Description: "Pause and ask the user to confirm before a destructive, hard-to-undo, or uncertain " +
			"action. Call this instead of guessing; do not perform the action in the same turn. " +
			"Defaults to a yes/no choice if options is omitted.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{` +
			`"summary":{"type":"string","description":"clear, specific description of the action awaiting approval"},` +
			`"options":{"type":"array","items":{"type":"object","properties":{` +
			`"label":{"type":"string"},"value":{"type":"string"}},"required":["label","value"]},` +
			`"description":"choices to offer; defaults to approve/reject"}},"required":["summary"]}`),
		Handler: func(_ context.Context, argsJSON string) (string, error) {
			var args confirmRequest
			if err := json.Unmarshal([]byte(argsJSON), &args); err != nil || args.Summary == "" {
				return "", fmt.Errorf("expected {summary, options?}")
			}
			if len(args.Options) == 0 {
				args.Options = []confirmOption{{Label: "同意", Value: "approve"}, {Label: "拒绝", Value: "reject"}}
			}
			slot.set(args)
			return "confirmation requested; stop here and wait for the user's decision in their next " +
				"message before proceeding — do not repeat or perform the action now.", nil
		},
	}
}

// newSessionConfig wires a fresh policy session plus this runtime's local
// tools (load_skill, bash, read_file, write_file, request_confirmation) into
// base, and returns the confirmSlot the caller must drain after each Send.
// See cmd/agent-runtime's newSessionConfig for why this must be per-session
// rather than shared: the tool guard, skill scope, and pending confirmation
// are all per-conversation state.
func newSessionConfig(base agentloop.Config, cfg *sdk.AgentConfig, enf *policy.Enforcer, root string, bashTimeout time.Duration) (agentloop.Config, *confirmSlot) {
	sess := enf.Session()
	slot := &confirmSlot{}
	base.ToolGuard = sess.Guard
	local := map[string]agentloop.LocalTool{
		"bash":                 bashTool(root, bashTimeout),
		"read_file":            readFileTool(root),
		"write_file":           writeFileTool(root),
		"request_confirmation": requestConfirmationTool(slot),
	}
	if tool, ok := loadSkillTool(cfg, sess); ok {
		local["load_skill"] = tool
	}
	base.LocalTools = local
	return base, slot
}

// buildSystemPrompt composes the PromptTemplate system text, a token-budgeted
// skill catalog (see estimateTokens), and the fixed coding-agent policy that
// tells the model to stop and ask before destructive or uncertain actions.
func buildSystemPrompt(cfg *sdk.AgentConfig, root string, logf func(string, ...any)) string {
	system := "You are a coding agent working in a real git checkout. Use the bash, " +
		"read_file, and write_file tools to inspect and change the code; do not just describe changes."
	if cfg.Prompt != nil && cfg.Prompt.System != "" {
		system = cfg.Prompt.System
	}

	catalog := make([]string, 0, len(cfg.Skills))
	totalTokens := 0
	for _, sk := range cfg.Skills {
		if sk.Content == "" {
			continue
		}
		desc := sk.Description
		if desc == "" {
			desc = sk.Name
		}
		catalog = append(catalog, fmt.Sprintf("- %s: %s", sk.Name, desc))
		tok := estimateTokens(sk.Content)
		totalTokens += tok
		logf("skill (catalog): %s (~%d tokens, lazy)", sk.Name, tok)
	}
	if len(catalog) > 0 {
		logf("skills: %d available, ~%d tokens total if all were loaded (only descriptions are in context now)",
			len(catalog), totalTokens)
		system += "\n\n# Skills available\n" +
			"The following skills are available but their full instructions are NOT loaded. " +
			"When a skill is relevant to the user's request, call load_skill(name) to load its " +
			"instructions before acting on that task.\n" +
			strings.Join(catalog, "\n")
	}

	confirmHint := "Call the request_confirmation tool with a clear, specific summary of what you " +
		"intend to do, then stop and wait — do not perform the action in the same turn."
	system += fmt.Sprintf(`

# Working in this repository
You have a persistent git checkout at %s. bash runs with that directory as its
working directory; read_file and write_file take paths relative to it.

Before doing anything destructive, hard to undo, or that you are not fully
confident matches what the user actually wants — including git push,
force-push, deleting files or branches, rewriting history, or changing
CI/CD, deployment, or credential configuration — stop first: %s
Once a later user message confirms or rejects it, continue accordingly rather
than repeating work you already did earlier in this conversation.

If a request is ambiguous or you are missing information needed to do it
correctly, ask a clarifying question instead of guessing.`, root, confirmHint)

	return system
}

// estimateTokens is a dependency-free approximation of token count: roughly 4
// ASCII characters per token (the common rule of thumb for BPE tokenizers on
// English text) and 1 token per non-ASCII rune (CJK and other wide scripts
// tokenize far closer to 1:1 than to 4:1). It exists so skill/context budget
// decisions are made in the unit the model's context window is actually
// measured in, not raw character counts — it is not a substitute for calling
// the provider's real tokenizer, which the SDK does not expose.
func estimateTokens(s string) int {
	ascii, wide := 0, 0
	for _, r := range s {
		if r < 128 {
			ascii++
		} else {
			wide++
		}
	}
	return ascii/4 + wide
}

// loadSkillTool mirrors cmd/agent-runtime's: it serves a named Skill's full
// instructions on demand and narrows the session's tool scope to the skill's
// AllowedTools once loaded. Kept in this binary too (rather than shared)
// because these are two independent runtime images, each free to diverge.
func loadSkillTool(cfg *sdk.AgentConfig, sess *policy.Session) (agentloop.LocalTool, bool) {
	skills := make(map[string]sdk.Skill, len(cfg.Skills))
	names := make([]string, 0, len(cfg.Skills))
	for _, sk := range cfg.Skills {
		if sk.Content == "" {
			continue
		}
		skills[sk.Name] = sk
		names = append(names, sk.Name)
	}
	if len(skills) == 0 {
		return agentloop.LocalTool{}, false
	}
	return agentloop.LocalTool{
		Description: "Load the full instructions for a named skill from the 'Skills available' " +
			"catalog in the system prompt. Call this before acting on a task the skill covers.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"name":{"type":"string",` +
			`"description":"skill name from the catalog"}},"required":["name"]}`),
		Handler: func(_ context.Context, argsJSON string) (string, error) {
			var args struct {
				Name string `json:"name"`
			}
			_ = json.Unmarshal([]byte(argsJSON), &args)
			sk, ok := skills[args.Name]
			if !ok {
				return fmt.Sprintf("no such skill %q; available skills: %s", args.Name, strings.Join(names, ", ")), nil
			}
			if sess != nil {
				sess.NoteSkillLoaded(sk.Name, sk.AllowedTools)
			}
			return sk.Content, nil
		},
	}, true
}

const (
	maxToolOutput = 8000   // bytes returned to the model per tool call, truncated beyond this
	maxReadBytes  = 300000 // largest file read_file will return whole
)

// resolveWorkspacePath confines rel to root: it rejects any path that
// resolves outside root, including via a symlinked ancestor directory. This
// is a second, independent check on top of the container-level sandbox (see
// the package doc comment) — it does not replace it, since bash is not
// confined the same way.
//
// root itself must already be canonical (symlinks resolved, as main() does
// once at startup) — this only guards against a symlink appearing *inside*
// the tree.
func resolveWorkspacePath(root, rel string) (string, error) {
	if rel == "" {
		return "", fmt.Errorf("path is required")
	}
	target := filepath.Clean(filepath.Join(root, rel))
	if target != root && !strings.HasPrefix(target, root+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q escapes the workspace root", rel)
	}
	// Walk up to the nearest existing ancestor and resolve symlinks on it, so a
	// symlinked directory inside the checkout cannot redirect us outside root
	// even for a file that does not exist yet (the write_file case).
	dir := filepath.Dir(target)
	for {
		real, err := filepath.EvalSymlinks(dir)
		if err == nil {
			if real != root && !strings.HasPrefix(real, root+string(filepath.Separator)) {
				return "", fmt.Errorf("path %q escapes the workspace root via a symlink", rel)
			}
			break
		}
		if !os.IsNotExist(err) {
			return "", err
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return target, nil
}

func bashTool(root string, timeout time.Duration) agentloop.LocalTool {
	return agentloop.LocalTool{
		Description: fmt.Sprintf("Run a shell command (via /bin/sh -c) with %s as the working directory. "+
			"Use this for git, build tools, test runners, and anything read_file/write_file cannot express. "+
			"Times out after %s.", root, timeout),
		InputSchema: json.RawMessage(`{"type":"object","properties":{"command":{"type":"string",` +
			`"description":"shell command to run"}},"required":["command"]}`),
		Handler: func(ctx context.Context, argsJSON string) (string, error) {
			var args struct {
				Command string `json:"command"`
			}
			if err := json.Unmarshal([]byte(argsJSON), &args); err != nil || args.Command == "" {
				return "", fmt.Errorf("expected {command}")
			}
			cctx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			cmd := exec.CommandContext(cctx, "/bin/sh", "-c", args.Command)
			cmd.Dir = root
			out, runErr := cmd.CombinedOutput()
			text := truncateBytes(string(out), maxToolOutput)
			status := "exit status 0"
			if runErr != nil {
				status = runErr.Error()
			}
			// The command's own failure is reported *in* the result, not as a
			// LocalTool error: a non-zero exit is routine information the model
			// needs to react to (e.g. a failing test), not a runtime fault.
			return fmt.Sprintf("$ %s\n%s\n(%s)", args.Command, text, status), nil
		},
	}
}

func readFileTool(root string) agentloop.LocalTool {
	return agentloop.LocalTool{
		Description: "Read a file by path relative to the working tree root.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string",` +
			`"description":"path relative to the checkout root"}},"required":["path"]}`),
		Handler: func(_ context.Context, argsJSON string) (string, error) {
			var args struct {
				Path string `json:"path"`
			}
			_ = json.Unmarshal([]byte(argsJSON), &args)
			abs, err := resolveWorkspacePath(root, args.Path)
			if err != nil {
				return "", err
			}
			data, err := os.ReadFile(abs)
			if err != nil {
				return fmt.Sprintf("cannot read %q: %v", args.Path, err), nil
			}
			if len(data) > maxReadBytes {
				return fmt.Sprintf("%s\n…(truncated at %d of %d bytes; read a narrower range with bash if you need more)",
					data[:maxReadBytes], maxReadBytes, len(data)), nil
			}
			return string(data), nil
		},
	}
}

func writeFileTool(root string) agentloop.LocalTool {
	return agentloop.LocalTool{
		Description: "Write a file by path relative to the working tree root, creating parent " +
			"directories as needed. Overwrites the file if it already exists.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"},` +
			`"content":{"type":"string"}},"required":["path","content"]}`),
		Handler: func(_ context.Context, argsJSON string) (string, error) {
			var args struct {
				Path    string `json:"path"`
				Content string `json:"content"`
			}
			if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
				return "", fmt.Errorf("expected {path, content}")
			}
			abs, err := resolveWorkspacePath(root, args.Path)
			if err != nil {
				return "", err
			}
			if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
				return "", err
			}
			if err := os.WriteFile(abs, []byte(args.Content), 0o644); err != nil {
				return "", err
			}
			return fmt.Sprintf("wrote %d bytes to %s", len(args.Content), args.Path), nil
		},
	}
}

func truncateBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + fmt.Sprintf("\n…(truncated at %d of %d bytes)", n, len(s))
}

// ensureGitIdentity sets a local git identity for the checkout so commits do
// not fail with "please tell me who you are". It is a no-op when root is not
// a git repository, and never overrides an identity already configured (e.g.
// by the user, or a previous run).
func ensureGitIdentity(root string, logf func(string, ...any)) {
	if _, err := os.Stat(filepath.Join(root, ".git")); err != nil {
		return
	}
	if out, err := exec.Command("git", "-C", root, "config", "user.name").Output(); err == nil && strings.TrimSpace(string(out)) != "" {
		return
	}
	name := envOr("GIT_AUTHOR_NAME", "agent-plane-bot")
	email := envOr("GIT_AUTHOR_EMAIL", "agent-plane-bot@users.noreply.github.com")
	if err := exec.Command("git", "-C", root, "config", "user.name", name).Run(); err != nil {
		logf("git identity: %v", err)
	}
	_ = exec.Command("git", "-C", root, "config", "user.email", email).Run()
}

func fatal(what string, err error) {
	fmt.Fprintf(os.Stderr, "✖ %s: %v\n", what, err)
	os.Exit(1)
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// openMemory returns a persistence Store for the first usable Memory, or nil.
func openMemory(ctx context.Context, sec *secrets.Reader, mems []sdk.Memory) memory.Store {
	for _, m := range mems {
		if sec == nil || m.SecretName == "" {
			continue
		}
		dsn, err := sec.Read(ctx, m.SecretName, m.SecretKey)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  memory %q: read connection secret: %v\n", m.Name, err)
			continue
		}
		store, err := memory.Open(m.Backend, dsn, m.Namespace)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  memory %q: %v\n", m.Name, err)
			continue
		}
		fmt.Printf("  memory: %s (backend=%s namespace=%s)\n", m.Name, m.Backend, m.Namespace)
		return store
	}
	return nil
}

func newRetriever(kbs []sdk.KnowledgeBase) *retriever.Retriever {
	r := retriever.New(kbs, retriever.WithLogf(func(f string, a ...any) {
		fmt.Fprintf(os.Stderr, "  "+f+"\n", a...)
	}))
	for _, kb := range kbs {
		fmt.Printf("  knowledgeBase: %s (source=%s uri=%s)\n", kb.Name, kb.Source, kb.URI)
	}
	return r
}

// chatREPL is a local-development convenience: a multi-turn conversation over
// stdin, restoring and persisting history through the configured Memory (if
// any) exactly like the serve-mode sessions do.
func chatREPL(ctx context.Context, name string, cfg *sdk.AgentConfig, base agentloop.Config, root string, bashTimeout time.Duration, store memory.Store, rag *retriever.Retriever, enf *policy.Enforcer) {
	sessionCfg, slot := newSessionConfig(base, cfg, enf, root, bashTimeout)
	session := agentloop.NewSession(sessionCfg)
	const sessionID = "cli"
	if store != nil {
		if turns, err := store.Load(ctx, sessionID); err == nil {
			for _, t := range turns {
				session.AppendHistory(t.Role, t.Content)
			}
			if len(turns) > 0 {
				fmt.Printf("(restored %d turns from memory)\n", len(turns))
			}
		}
	}
	fmt.Printf("\n💬 Chatting with coding agent %q (workspace %s). 'exit' or Ctrl-D to quit.\n\n", name, root)
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	fmt.Print("you> ")
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		switch line {
		case "":
			fmt.Print("you> ")
			continue
		case "exit", "quit":
			fmt.Println("bye")
			return
		}
		answer, confirm, err := sendAndConfirm(ctx, session, slot, rag.Augment(ctx, line))
		if err != nil {
			fmt.Printf("✖ %v\n", err)
		} else {
			fmt.Printf("agent> %s\n", answer)
			if confirm != nil {
				fmt.Printf("  [confirmation requested] %s\n", confirm.Summary)
				for _, o := range confirm.Options {
					fmt.Printf("    - %s (%s)\n", o.Label, o.Value)
				}
			}
			if store != nil {
				_ = store.Append(ctx, sessionID,
					memory.Turn{Role: "user", Content: line},
					memory.Turn{Role: "assistant", Content: answer})
			}
		}
		fmt.Print("\nyou> ")
	}
	fmt.Println()
}

// serveHTTP is how a Trigger-driven pod runs: /api/chat is exactly the
// adapter-protocol contract (docs/adapter-protocol.md) an inbound adapter
// (e.g. cmd/lark-adapter) POSTs to. Sessions are keyed by the caller's
// sessionId (an IM chat id), each with its own tool-call budget and skill
// scope, and — when a Memory is configured — its own persisted history, so a
// confirmation reply in the same chat continues the same conversation the
// pending action was proposed in.
func serveHTTP(ctx context.Context, name string, cfg *sdk.AgentConfig, base agentloop.Config, root string, bashTimeout time.Duration, addr string, store memory.Store, rag *retriever.Retriever, enf *policy.Enforcer) {
	var mu sync.Mutex
	type session struct {
		agent *agentloop.Session
		slot  *confirmSlot
		// turn serializes access to agent.Send: it is not safe for concurrent
		// use, and without this, two requests hitting the same sessionId at once
		// (an IM platform redelivering a card click while the first delivery is
		// still being handled, say) would race on the same conversation history.
		turn sync.Mutex
	}
	sessions := map[string]*session{}
	getSession := func(reqCtx context.Context, id string) *session {
		mu.Lock()
		defer mu.Unlock()
		s, ok := sessions[id]
		if !ok {
			sessionCfg, slot := newSessionConfig(base, cfg, enf, root, bashTimeout)
			agent := agentloop.NewSession(sessionCfg)
			if store != nil {
				if turns, err := store.Load(reqCtx, id); err == nil {
					for _, t := range turns {
						agent.AppendHistory(t.Role, t.Content)
					}
				}
			}
			s = &session{agent: agent, slot: slot}
			sessions[id] = s
		}
		return s
	}

	toolNames := []string{"bash", "read_file", "write_file", "request_confirmation"}
	for _, t := range cfg.Tools {
		toolNames = append(toolNames, t.Name)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	mux.HandleFunc("/api/info", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"agent": name, "model": cfg.Model.ModelName, "workspace": root, "tools": toolNames})
	})
	mux.HandleFunc("/api/chat", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			SessionID string `json:"sessionId"`
			Message   string `json:"message"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Message == "" {
			http.Error(w, "expected {sessionId, message}", http.StatusBadRequest)
			return
		}
		if req.SessionID == "" {
			req.SessionID = "default"
		}
		sess := getSession(r.Context(), req.SessionID)
		// TryLock, not Lock: without this, a second message arriving in the same
		// chat while a slow turn (a big repo exploration can run many tool-call
		// rounds) is still in flight would queue up silently behind sess.turn and
		// then also blow its own timeout on the adapter side — the user sees two
		// timeouts and no actual answer, exactly what happened before this. Fail
		// fast instead so the adapter can tell them a turn is already running.
		if !sess.turn.TryLock() {
			writeJSON(w, map[string]any{"answer": "上一个请求还在处理中，请稍候再问一次。"})
			return
		}
		answer, confirm, err := sendAndConfirm(r.Context(), sess.agent, sess.slot, rag.Augment(r.Context(), req.Message))
		sess.turn.Unlock()
		if err != nil {
			writeJSON(w, map[string]any{"error": err.Error()})
			return
		}
		if store != nil {
			_ = store.Append(r.Context(), req.SessionID,
				memory.Turn{Role: "user", Content: req.Message},
				memory.Turn{Role: "assistant", Content: answer})
		}
		resp := map[string]any{"answer": answer}
		// confirmation is additive on top of the adapter-protocol /api/chat
		// contract: an adapter that doesn't know the field just shows answer,
		// which already describes the pending action in prose.
		if confirm != nil {
			resp["confirmation"] = confirm
		}
		writeJSON(w, resp)
	})

	fmt.Printf("▶ coding-agent %q serving on %s (workspace=%s model=%s tools=%v)\n", name, addr, root, cfg.Model.ModelName, toolNames)
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { <-ctx.Done(); _ = srv.Close() }()
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		fatal("http server", err)
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// watchLoop is the long-running data-plane mode: subscribe to the Registry's
// SSE stream and hot-reload. Identical in shape to cmd/agent-runtime's; see
// there for why this, not a poll loop, is how a deployed pod stays current.
func watchLoop(ctx context.Context, rc *sdk.Client, ns, name string) {
	sec, err := secrets.NewReader(ns)
	if err != nil {
		fmt.Fprintf(os.Stderr, "  no kubernetes access (%v); hot-reload will skip Secret reads\n", err)
	}
	fmt.Printf("▶ coding-agent watching the Registry for %s/%s\n", ns, name)
	_ = rc.Watch(ctx, ns, name, func(cfg *sdk.AgentConfig) {
		applyConfig(ctx, sec, cfg)
	})
}

func applyConfig(ctx context.Context, sec *secrets.Reader, cfg *sdk.AgentConfig) {
	model := "<none>"
	keyLen := 0
	if cfg.Model != nil {
		model = cfg.Model.Provider + "/" + cfg.Model.ModelName
		if sec != nil && cfg.Model.SecretName != "" {
			if v, err := sec.Read(ctx, cfg.Model.SecretName, cfg.Model.SecretKey); err == nil {
				keyLen = len(v)
			}
		}
	}
	toolNames := make([]string, 0, len(cfg.Tools))
	for _, t := range cfg.Tools {
		toolNames = append(toolNames, fmt.Sprintf("%s(%s)", t.Name, t.Type))
	}
	skillNames := make([]string, 0, len(cfg.Skills))
	for _, sk := range cfg.Skills {
		skillNames = append(skillNames, sk.Name)
	}
	policySummary := "none"
	if lines := policy.New(cfg.Policy).Describe(); len(lines) > 0 {
		policySummary = strings.Join(lines, "; ")
	}
	fmt.Printf("↻ hot-reload: phase=%s hash=%.12s model=%s keyLen=%d tools=%v skills=%v policy=[%s]\n",
		cfg.Phase, cfg.ConfigHash, model, keyLen, toolNames, skillNames, policySummary)
}
