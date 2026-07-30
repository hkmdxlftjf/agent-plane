package main

import (
	"context"
	"strings"
	"testing"

	sdk "github.com/hkmdxlftjf/agent-plane-sdk-go"
	"github.com/hkmdxlftjf/agent-plane-sdk-go/agentloop"
	"github.com/hkmdxlftjf/agent-plane-sdk-go/policy"
)

func testCfg() *sdk.AgentConfig {
	return &sdk.AgentConfig{
		Prompt: &sdk.Prompt{System: "BASE SYSTEM"},
		Skills: []sdk.Skill{
			{Name: "refunds", Description: "process a refund", Content: "STEP1 issue refund via api"},
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
	tool, ok := loadSkillTool(testCfg())
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
	if !strings.Contains(miss, "no such skill") || !strings.Contains(miss, "refunds") {
		t.Fatalf("unknown-name result should list available skills, got %q", miss)
	}
}

// No loadable skill -> no tool registered.
func TestLoadSkillToolNone(t *testing.T) {
	cfg := &sdk.AgentConfig{Skills: []sdk.Skill{{Name: "x", Content: ""}}}
	if _, ok := loadSkillTool(cfg); ok {
		t.Fatal("expected ok=false when no skill has content")
	}
}

// withSessionGuard must hand every session its OWN counters. Sharing one guard
// would make a tool's maxCallsPerSession budget global, so the second
// conversation would inherit the first one's exhausted quota.
func TestWithSessionGuardIsolatesSessions(t *testing.T) {
	limit := int32(1)
	enf := policy.New(&sdk.Policy{ToolRules: []sdk.ToolRule{
		{Tool: "refund", Action: sdk.ToolActionAllow, MaxCallsPerSession: &limit},
	}})

	first := withSessionGuard(agentloop.Config{}, enf)
	if err := first.ToolGuard("refund"); err != nil {
		t.Fatalf("first call in session 1: %v", err)
	}
	if err := first.ToolGuard("refund"); err == nil {
		t.Fatal("second call in session 1 should have exceeded the cap")
	}

	second := withSessionGuard(agentloop.Config{}, enf)
	if err := second.ToolGuard("refund"); err != nil {
		t.Errorf("first call in session 2: %v, want a fresh budget", err)
	}
}

// A denied tool must be refused by the wired guard, and the reason must be
// legible enough for the model (and an operator reading logs) to act on.
func TestWithSessionGuardDeniesDeniedTool(t *testing.T) {
	enf := policy.New(&sdk.Policy{
		Sources: []string{"Policy/guardrails"},
		Tools:   &sdk.AccessRule{Deny: []string{"refund"}},
	})
	cfg := withSessionGuard(agentloop.Config{}, enf)

	err := cfg.ToolGuard("refund")
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
func TestWithSessionGuardNoPolicy(t *testing.T) {
	cfg := withSessionGuard(agentloop.Config{}, policy.New(nil))
	if cfg.ToolGuard == nil {
		t.Fatal("ToolGuard is nil; want an installed no-op guard")
	}
	if err := cfg.ToolGuard("anything"); err != nil {
		t.Errorf("ToolGuard = %v, want nil", err)
	}
}

// The guard must not disturb the rest of the config it copies — notably the
// load_skill LocalTool, which shares the same base.
func TestWithSessionGuardPreservesBase(t *testing.T) {
	tool, ok := loadSkillTool(testCfg())
	if !ok {
		t.Fatal("loadSkillTool returned no tool")
	}
	base := agentloop.Config{
		Model:      "m",
		MaxSteps:   8,
		LocalTools: map[string]agentloop.LocalTool{"load_skill": tool},
	}
	got := withSessionGuard(base, policy.New(nil))
	if got.Model != "m" || got.MaxSteps != 8 {
		t.Errorf("base fields altered: %+v", got)
	}
	if _, ok := got.LocalTools["load_skill"]; !ok {
		t.Error("load_skill LocalTool was dropped")
	}
}
