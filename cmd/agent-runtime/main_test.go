package main

import (
	"context"
	"strings"
	"testing"

	sdk "github.com/hkmdxlftjf/agent-plane-sdk-go"
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
