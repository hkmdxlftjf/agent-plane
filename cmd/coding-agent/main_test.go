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
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sdk "github.com/hkmdxlftjf/agent-plane-sdk-go"
)

func canonicalTempDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("resolve symlinks for temp dir: %v", err)
	}
	return real
}

func TestResolveWorkspacePathConfinesToRoot(t *testing.T) {
	root := canonicalTempDir(t)
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name    string
		path    string
		wantErr bool
	}{
		{"plain relative", "sub/file.go", false},
		{"dot-dot escape", "../etc/passwd", true},
		{"nested dot-dot escape", "sub/../../etc/passwd", true},
		{"empty path", "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			abs, err := resolveWorkspacePath(root, c.path)
			if c.wantErr {
				if err == nil {
					t.Fatalf("resolveWorkspacePath(%q): expected error, got abs=%q", c.path, abs)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveWorkspacePath(%q): unexpected error: %v", c.path, err)
			}
			if !strings.HasPrefix(abs, root) {
				t.Fatalf("resolved path %q escapes root %q", abs, root)
			}
		})
	}
}

func TestResolveWorkspacePathRejectsSymlinkEscape(t *testing.T) {
	root := canonicalTempDir(t)
	outside := canonicalTempDir(t)
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Skipf("symlinks unsupported in this environment: %v", err)
	}
	if _, err := resolveWorkspacePath(root, "escape/file.txt"); err == nil {
		t.Fatal("expected an error for a path under a symlinked escape, got nil")
	}
}

func TestReadWriteFileToolsRoundtrip(t *testing.T) {
	root := canonicalTempDir(t)
	ctx := context.Background()

	write := writeFileTool(root)
	out, err := write.Handler(ctx, `{"path":"a/b.txt","content":"hello"}`)
	if err != nil {
		t.Fatalf("write_file: %v", err)
	}
	if !strings.Contains(out, "wrote 5 bytes") {
		t.Fatalf("unexpected write_file result: %q", out)
	}

	read := readFileTool(root)
	got, err := read.Handler(ctx, `{"path":"a/b.txt"}`)
	if err != nil {
		t.Fatalf("read_file: %v", err)
	}
	if got != "hello" {
		t.Fatalf("read_file: got %q, want %q", got, "hello")
	}

	if _, err := write.Handler(ctx, `{"path":"../outside.txt","content":"x"}`); err == nil {
		t.Fatal("write_file: expected an error escaping the root, got nil")
	}
}

func TestBashToolRunsInWorkspaceAndReportsExitStatus(t *testing.T) {
	root := canonicalTempDir(t)
	if err := os.WriteFile(filepath.Join(root, "marker.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	tool := bashTool(root, 5*time.Second)

	out, err := tool.Handler(context.Background(), `{"command":"ls"}`)
	if err != nil {
		t.Fatalf("bash: %v", err)
	}
	if !strings.Contains(out, "marker.txt") {
		t.Fatalf("bash output missing workspace file: %q", out)
	}
	if !strings.Contains(out, "exit status 0") {
		t.Fatalf("bash output missing success status: %q", out)
	}

	// A failing command is reported in the result text, not as a LocalTool
	// error: the model needs to see and react to it, not have the run abort.
	out, err = tool.Handler(context.Background(), `{"command":"exit 3"}`)
	if err != nil {
		t.Fatalf("bash: %v", err)
	}
	if strings.Contains(out, "exit status 0") {
		t.Fatalf("bash output should report the non-zero exit: %q", out)
	}
}

func TestBashToolTimesOut(t *testing.T) {
	root := canonicalTempDir(t)
	tool := bashTool(root, 50*time.Millisecond)
	out, err := tool.Handler(context.Background(), `{"command":"sleep 5"}`)
	if err != nil {
		t.Fatalf("bash: %v", err)
	}
	if strings.Contains(out, "exit status 0") {
		t.Fatalf("expected a timeout/kill status, got: %q", out)
	}
}

func TestEstimateTokensWeighsWideRunesHigher(t *testing.T) {
	ascii := estimateTokens(strings.Repeat("a", 400)) // ~400/4 = 100
	if ascii < 90 || ascii > 110 {
		t.Fatalf("ascii estimate = %d, want ~100", ascii)
	}
	cjk := estimateTokens(strings.Repeat("中", 100)) // ~1 token/rune = 100
	if cjk < 95 {
		t.Fatalf("cjk estimate = %d, want ~100", cjk)
	}
	if cjk <= ascii/2 {
		t.Fatalf("expected cjk estimate (%d) to weigh noticeably higher per-rune than ascii (%d)", cjk, ascii)
	}
}

func TestLoadSkillToolServesContentAndNarrowsScope(t *testing.T) {
	cfg := &sdk.AgentConfig{
		Skills: []sdk.Skill{{Name: "demo", Description: "a demo skill", Content: "full instructions"}},
	}
	tool, ok := loadSkillTool(cfg, nil)
	if !ok {
		t.Fatal("expected loadSkillTool to register a tool")
	}
	out, err := tool.Handler(context.Background(), `{"name":"demo"}`)
	if err != nil {
		t.Fatalf("load_skill: %v", err)
	}
	if out != "full instructions" {
		t.Fatalf("load_skill returned %q, want the skill content", out)
	}

	out, err = tool.Handler(context.Background(), `{"name":"missing"}`)
	if err != nil {
		t.Fatalf("load_skill: %v", err)
	}
	if !strings.Contains(out, "no such skill") {
		t.Fatalf("expected an unknown-skill message, got %q", out)
	}
}

func TestBuildSystemPromptMentionsRequestConfirmationTool(t *testing.T) {
	cfg := &sdk.AgentConfig{}
	system := buildSystemPrompt(cfg, "/workspace", func(string, ...any) {})
	if !strings.Contains(system, "request_confirmation") {
		t.Fatalf("system prompt should always mention request_confirmation:\n%s", system)
	}
}

func TestRequestConfirmationToolRecordsAndClearsPending(t *testing.T) {
	slot := &confirmSlot{}
	tool := requestConfirmationTool(slot)

	if _, err := tool.Handler(context.Background(), `{"summary":"push to main"}`); err != nil {
		t.Fatalf("request_confirmation: %v", err)
	}
	c := slot.takeAndClear()
	if c == nil || c.Summary != "push to main" {
		t.Fatalf("expected a pending confirmation with the given summary, got %+v", c)
	}
	if len(c.Options) != 2 || c.Options[0].Value != "approve" || c.Options[1].Value != "reject" {
		t.Fatalf("expected default approve/reject options, got %+v", c.Options)
	}
	if slot.takeAndClear() != nil {
		t.Fatal("takeAndClear should clear the pending confirmation")
	}

	if _, err := tool.Handler(context.Background(), `{}`); err == nil {
		t.Fatal("expected an error when summary is missing")
	}
}

func TestConfirmSlotSetCancelsArmedSend(t *testing.T) {
	slot := &confirmSlot{}
	canceled := false
	slot.arm(func() { canceled = true })

	slot.set(confirmRequest{Summary: "push to main"})

	if !canceled {
		t.Fatal("set should call the armed cancel func, forcing the in-flight Send to stop")
	}
	if c := slot.takeAndClear(); c == nil || c.Summary != "push to main" {
		t.Fatalf("expected the confirmation to still be recorded, got %+v", c)
	}

	// A second set (a stray/duplicate call) must not panic on an
	// already-cleared cancel func.
	slot.set(confirmRequest{Summary: "again"})
}
