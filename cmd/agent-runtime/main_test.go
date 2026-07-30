package main

import (
	"context"
	"strings"
	"testing"

	sdk "github.com/hkmdxlftjf/agent-plane-sdk-go"
	"github.com/hkmdxlftjf/agent-plane-sdk-go/agentloop"
	"github.com/hkmdxlftjf/agent-plane-sdk-go/policy"
)

// Fixture literals shared across these tests.
const (
	skillRefunds = "refunds"
	toolRefund   = "refund"
)

func testCfg() *sdk.AgentConfig {
	return &sdk.AgentConfig{
		Prompt: &sdk.Prompt{System: "BASE SYSTEM"},
		Skills: []sdk.Skill{
			{Name: skillRefunds, Description: "process a refund", Content: "STEP1 issue refund via api"},
			{Name: "escalate", Description: "", Content: "call the manager"},
			{Name: "empty", Description: "no body", Content: ""}, // must be skipped
		},
	}
}

// buildSystemPrompt must inline only the catalog (name + description), never a
// skill's full body.
func TestBuildSystemPromptCatalogOnly(t *testing.T) {
	sys := buildSystemPrompt(testCfg(), func(string, ...any) {})

	if !strings.Contains(sys, "BASE SYSTEM") {
		t.Fatal("base system prompt missing")
	}
	if !strings.Contains(sys, "# Skills available") {
		t.Fatal("catalog header missing")
	}
	if !strings.Contains(sys, "- refunds: process a refund") {
		t.Fatalf("refunds catalog line missing:\n%s", sys)
	}
	// description empty -> falls back to name.
	if !strings.Contains(sys, "- escalate: escalate") {
		t.Fatalf("escalate fallback line missing:\n%s", sys)
	}
	// Full bodies must NOT be inlined.
	if strings.Contains(sys, "STEP1 issue refund") || strings.Contains(sys, "call the manager") {
		t.Fatalf("skill body leaked into system prompt:\n%s", sys)
	}
	// Content-less skill must not appear.
	if strings.Contains(sys, "empty") {
		t.Fatalf("content-less skill should be excluded:\n%s", sys)
	}
}

// loadSkillTool must serve full bodies on demand and reject unknown names.
func TestLoadSkillTool(t *testing.T) {
	tool, ok := loadSkillTool(testCfg(), policy.New(nil).Session())
	if !ok {
		t.Fatal("expected a load_skill tool for a config with loadable skills")
	}

	body, err := tool.Handler(context.Background(), `{"name":"refunds"}`)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if body != "STEP1 issue refund via api" {
		t.Fatalf("handler returned %q, want the full body", body)
	}

	miss, err := tool.Handler(context.Background(), `{"name":"nope"}`)
	if err != nil {
		t.Fatalf("unknown-name should not error: %v", err)
	}
	if !strings.Contains(miss, "no such skill") || !strings.Contains(miss, skillRefunds) {
		t.Fatalf("unknown-name result should list available skills, got %q", miss)
	}
}

// No loadable skill -> no tool registered.
func TestLoadSkillToolNone(t *testing.T) {
	cfg := &sdk.AgentConfig{Skills: []sdk.Skill{{Name: "x", Content: ""}}}
	if _, ok := loadSkillTool(cfg, policy.New(nil).Session()); ok {
		t.Fatal("expected ok=false when no skill has content")
	}
}

// newSessionConfig must hand every session its OWN counters. Sharing one guard
// would make a tool's maxCallsPerSession budget global, so the second
// conversation would inherit the first one's exhausted quota.
func TestNewSessionConfigIsolatesSessions(t *testing.T) {
	limit := int32(1)
	enf := policy.New(&sdk.Policy{ToolRules: []sdk.ToolRule{
		{Tool: toolRefund, Action: sdk.ToolActionAllow, MaxCallsPerSession: &limit},
	}})

	first := newSessionConfig(agentloop.Config{}, testCfg(), enf)
	if err := first.ToolGuard(toolRefund); err != nil {
		t.Fatalf("first call in session 1: %v", err)
	}
	if err := first.ToolGuard(toolRefund); err == nil {
		t.Fatal("second call in session 1 should have exceeded the cap")
	}

	second := newSessionConfig(agentloop.Config{}, testCfg(), enf)
	if err := second.ToolGuard(toolRefund); err != nil {
		t.Errorf("first call in session 2: %v, want a fresh budget", err)
	}
}

// A denied tool must be refused by the wired guard, and the reason must be
// legible enough for the model (and an operator reading logs) to act on.
func TestNewSessionConfigDeniesDeniedTool(t *testing.T) {
	enf := policy.New(&sdk.Policy{
		Sources: []string{"Policy/guardrails"},
		Tools:   &sdk.AccessRule{Deny: []string{toolRefund}},
	})
	cfg := newSessionConfig(agentloop.Config{}, testCfg(), enf)

	err := cfg.ToolGuard(toolRefund)
	if err == nil {
		t.Fatal("ToolGuard(refund) = nil, want a denial")
	}
	if !strings.Contains(err.Error(), "guardrails") {
		t.Errorf("denial %q does not name the Policy source", err)
	}
	if err := cfg.ToolGuard("lookup"); err != nil {
		t.Errorf("ToolGuard(lookup) = %v, want nil", err)
	}
}

// With no Policy referenced the guard must still be installed and permissive,
// so the wiring has one code path rather than two.
func TestNewSessionConfigNoPolicy(t *testing.T) {
	cfg := newSessionConfig(agentloop.Config{}, testCfg(), policy.New(nil))
	if cfg.ToolGuard == nil {
		t.Fatal("ToolGuard is nil; want an installed no-op guard")
	}
	if err := cfg.ToolGuard("anything"); err != nil {
		t.Errorf("ToolGuard = %v, want nil", err)
	}
}

// The guard must not disturb the rest of the config it copies — notably the
// load_skill LocalTool, which shares the same base.
func TestNewSessionConfigPreservesBase(t *testing.T) {
	base := agentloop.Config{
		Model:    "m",
		MaxSteps: 8,
		System:   "SYS",
	}
	got := newSessionConfig(base, testCfg(), policy.New(nil))
	if got.Model != "m" || got.MaxSteps != 8 || got.System != "SYS" {
		t.Errorf("base fields altered: %+v", got)
	}
	// newSessionConfig owns load_skill: it must install one bound to this
	// session's scope, not inherit a shared instance.
	if _, ok := got.LocalTools["load_skill"]; !ok {
		t.Error("load_skill LocalTool was not installed")
	}
	// The caller's base must be left untouched, since it is shared across
	// sessions — mutating it would leak one session's tool into the next.
	if base.LocalTools != nil {
		t.Error("newSessionConfig mutated the shared base")
	}
}

// An Agent with no loadable skill gets no load_skill tool, and the guard still
// works — the two are independent.
func TestNewSessionConfigWithoutSkills(t *testing.T) {
	cfg := &sdk.AgentConfig{Skills: []sdk.Skill{{Name: "x", Content: ""}}}
	got := newSessionConfig(agentloop.Config{}, cfg, policy.New(nil))
	if len(got.LocalTools) != 0 {
		t.Errorf("LocalTools = %v, want none", got.LocalTools)
	}
	if got.ToolGuard == nil {
		t.Error("ToolGuard should still be installed")
	}
}

// Loading a skill that declares allowedTools must narrow the session from that
// point on. This is the whole point of binding load_skill to the session.
func TestLoadSkillNarrowsSessionScope(t *testing.T) {
	cfg := &sdk.AgentConfig{Skills: []sdk.Skill{{
		Name:         skillRefunds,
		Content:      "refund instructions",
		AllowedTools: []string{toolRefund},
	}}}
	got := newSessionConfig(agentloop.Config{}, cfg, policy.New(nil))

	// Before the skill loads nothing is restricted.
	if err := got.ToolGuard("delete-account"); err != nil {
		t.Fatalf("before load: ToolGuard = %v, want unrestricted", err)
	}

	if _, err := got.LocalTools["load_skill"].Handler(context.Background(), `{"name":"refunds"}`); err != nil {
		t.Fatalf("load_skill: %v", err)
	}

	if err := got.ToolGuard(toolRefund); err != nil {
		t.Errorf("after load: ToolGuard(refund) = %v, want allowed", err)
	}
	err := got.ToolGuard("delete-account")
	if err == nil {
		t.Fatal("after load: a tool outside allowedTools should be refused")
	}
	if !strings.Contains(err.Error(), skillRefunds) {
		t.Errorf("refusal %q should name the loaded skill", err)
	}
}

// A skill without allowedTools must not restrict anything when loaded — most
// skills are pure instructions.
func TestLoadSkillWithoutAllowedToolsDoesNotNarrow(t *testing.T) {
	cfg := &sdk.AgentConfig{Skills: []sdk.Skill{{Name: "prose", Content: "just advice"}}}
	got := newSessionConfig(agentloop.Config{}, cfg, policy.New(nil))

	if _, err := got.LocalTools["load_skill"].Handler(context.Background(), `{"name":"prose"}`); err != nil {
		t.Fatalf("load_skill: %v", err)
	}
	if err := got.ToolGuard("anything"); err != nil {
		t.Errorf("ToolGuard = %v, want unrestricted", err)
	}
}

// One conversation loading a narrow skill must not confine another.
func TestLoadSkillScopeIsPerSession(t *testing.T) {
	cfg := &sdk.AgentConfig{Skills: []sdk.Skill{{
		Name:         skillRefunds,
		Content:      "refund instructions",
		AllowedTools: []string{toolRefund},
	}}}
	enf := policy.New(nil)

	first := newSessionConfig(agentloop.Config{}, cfg, enf)
	if _, err := first.LocalTools["load_skill"].Handler(context.Background(), `{"name":"refunds"}`); err != nil {
		t.Fatalf("load_skill: %v", err)
	}
	if err := first.ToolGuard("delete-account"); err == nil {
		t.Fatal("first session should be scoped after loading the skill")
	}

	second := newSessionConfig(agentloop.Config{}, cfg, enf)
	if err := second.ToolGuard("delete-account"); err != nil {
		t.Errorf("second session: ToolGuard = %v, want an unrestricted scope", err)
	}
}
