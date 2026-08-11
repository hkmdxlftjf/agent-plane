package main

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher/callback"
)

const (
	testChatID  = "oc_chat_123"
	testAgentEP = "http://agent:8080"
	testAppID   = "cli_test"
	testSecret  = "secret_test"
)

// writeCredential lays out the Secret the way the Operator mounts it: one file
// per key, not environment values.
func writeCredential(t *testing.T, keys map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for k, v := range keys {
		if err := os.WriteFile(filepath.Join(dir, k), []byte(v), 0o600); err != nil {
			t.Fatalf("write %s: %v", k, err)
		}
	}
	return dir
}

func TestLoadConfig(t *testing.T) {
	dir := writeCredential(t, map[string]string{
		// A trailing newline is what `kubectl create secret --from-file` and most
		// editors produce; carrying it into the app id makes Lark reject the
		// handshake with an error that does not mention whitespace.
		keyAppID:     testAppID + "\n",
		keyAppSecret: testSecret,
	})
	t.Setenv("AGENTPLANE_AGENT_ENDPOINT", "http://agent.default.svc:8080")
	t.Setenv("AGENTPLANE_AGENT_NAME", "support-agent")
	t.Setenv("AGENTPLANE_CREDENTIAL_PATH", dir)
	t.Setenv("AGENTPLANE_TRIGGER_CONFIG", `{"replyInThread":true,"timeoutSeconds":90}`)

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.AppID != testAppID {
		t.Errorf("AppID = %q, want the value trimmed of whitespace", cfg.AppID)
	}
	if !cfg.ReplyInThread {
		t.Error("replyInThread from spec.config was ignored")
	}
	if cfg.Timeout != 90*time.Second {
		t.Errorf("Timeout = %v, want 90s from spec.config", cfg.Timeout)
	}
}

// spec.config is optional, and its absence must leave the defaults intact rather
// than zeroing the timeout — a zero timeout fails every request instantly.
func TestLoadConfigDefaults(t *testing.T) {
	dir := writeCredential(t, map[string]string{keyAppID: testAppID, keyAppSecret: testSecret})
	t.Setenv("AGENTPLANE_AGENT_ENDPOINT", testAgentEP)
	t.Setenv("AGENTPLANE_CREDENTIAL_PATH", dir)
	t.Setenv("AGENTPLANE_TRIGGER_CONFIG", "")

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.Timeout <= 0 {
		t.Errorf("Timeout = %v, want a usable default", cfg.Timeout)
	}
	// A thread per answer turns a one-on-one chat into collapsed branches, and
	// the reply is anchored to its message without one.
	if cfg.ReplyInThread {
		t.Error("ReplyInThread should default to false")
	}
}

// Every one of these is a misconfiguration that would otherwise surface as a
// confusing runtime failure long after apply.
func TestLoadConfigRejectsIncompleteWiring(t *testing.T) {
	full := map[string]string{keyAppID: testAppID, keyAppSecret: testSecret}

	tests := []struct {
		name     string
		endpoint string
		creds    map[string]string
		noCred   bool
		wantIn   string
	}{
		{
			name:   "no endpoint means the Agent has no runtime port",
			creds:  full,
			wantIn: "AGENTPLANE_AGENT_ENDPOINT",
		},
		{
			name:     "no credential mount",
			endpoint: testAgentEP,
			noCred:   true,
			wantIn:   "AGENTPLANE_CREDENTIAL_PATH",
		},
		{
			name:     "right Secret, wrong keys",
			endpoint: testAgentEP,
			creds:    map[string]string{"token": "x"},
			wantIn:   keyAppID,
		},
		{
			name:     "an empty key is as broken as a missing one",
			endpoint: testAgentEP,
			creds:    map[string]string{keyAppID: testAppID, keyAppSecret: "   "},
			wantIn:   keyAppSecret,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("AGENTPLANE_AGENT_ENDPOINT", tc.endpoint)
			t.Setenv("AGENTPLANE_TRIGGER_CONFIG", "")
			if tc.noCred {
				t.Setenv("AGENTPLANE_CREDENTIAL_PATH", "")
			} else {
				t.Setenv("AGENTPLANE_CREDENTIAL_PATH", writeCredential(t, tc.creds))
			}

			_, err := loadConfig()
			if err == nil {
				t.Fatal("expected an error naming what is missing")
			}
			if !strings.Contains(err.Error(), tc.wantIn) {
				t.Errorf("error = %q, want it to name %q", err, tc.wantIn)
			}
		})
	}
}

// Rule 2 is the one field with real consequences: the platform's conversation id
// becomes the sessionId, so a chat gets multi-turn memory and two chats never
// share history.
func TestAskUsesTheConversationIDAsSession(t *testing.T) {
	var got chatRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/chat" {
			t.Errorf("path = %q, want /api/chat", r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		_ = json.NewEncoder(w).Encode(chatResponse{Answer: "hello"})
	}))
	defer srv.Close()

	a := &adapter{cfg: &config{Endpoint: srv.URL, Timeout: time.Minute}, http: srv.Client()}
	answer, err := a.ask(context.Background(), testChatID, "hi")
	if err != nil {
		t.Fatalf("ask: %v", err)
	}
	if answer.Answer != "hello" {
		t.Errorf("answer = %q, want %q", answer.Answer, "hello")
	}
	if got.SessionID != testChatID {
		t.Errorf("sessionId = %q, want the chat id", got.SessionID)
	}
	if got.Message != "hi" {
		t.Errorf("message = %q, want the user's text", got.Message)
	}
}

// A trailing slash on the injected endpoint must not produce //api/chat, which
// some routers treat as a different path.
func TestAskNormalizesTheEndpoint(t *testing.T) {
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		_ = json.NewEncoder(w).Encode(chatResponse{Answer: "ok"})
	}))
	defer srv.Close()

	a := &adapter{cfg: &config{Endpoint: srv.URL + "/", Timeout: time.Minute}, http: srv.Client()}
	if _, err := a.ask(context.Background(), testChatID, "hi"); err != nil {
		t.Fatalf("ask: %v", err)
	}
	if path != "/api/chat" {
		t.Errorf("path = %q, want /api/chat", path)
	}
}

// The contract returns runtime failures in the body with HTTP 200, so checking
// only the status code would report a failed turn as a successful one.
func TestAskTreatsAnErrorBodyAsFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(chatResponse{Error: "model unavailable"})
	}))
	defer srv.Close()

	a := &adapter{cfg: &config{Endpoint: srv.URL, Timeout: time.Minute}, http: srv.Client()}
	_, err := a.ask(context.Background(), testChatID, "hi")
	if err == nil {
		t.Fatal("an error body with HTTP 200 was reported as success")
	}
	if !strings.Contains(err.Error(), "model unavailable") {
		t.Errorf("error = %q, want the runtime's message", err)
	}
}

// An empty answer would post a blank message into the chat, which reads as the
// bot ignoring the user.
func TestAskRejectsAnEmptyAnswer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(chatResponse{})
	}))
	defer srv.Close()

	a := &adapter{cfg: &config{Endpoint: srv.URL, Timeout: time.Minute}, http: srv.Client()}
	if _, err := a.ask(context.Background(), testChatID, "hi"); err == nil {
		t.Fatal("an empty answer was accepted")
	}
}

func TestExtractText(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    string
	}{
		{"a text message", `{"text":"how do I deploy?"}`, "how do I deploy?"},
		{"surrounding whitespace is dropped", `{"text":"  hi  "}`, "hi"},
		// Forwarding an unparseable payload raw would hand the model a blob of
		// JSON to reason about instead of a question.
		{"malformed content is skipped", `not json`, ""},
		{"an empty message is skipped", `{"text":""}`, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := extractText(tc.content); got != tc.want {
				t.Errorf("extractText(%q) = %q, want %q", tc.content, got, tc.want)
			}
		})
	}
}

// The readiness probe must answer while the process is alive. It intentionally
// reports only that much: the contract is explicit that a Running Trigger means
// "the process exists", not that Lark accepted the connection — only the adapter
// knows the latter, and nothing reports it back.
func TestHealthEndpoint(t *testing.T) {
	a := &adapter{cfg: &config{HealthAddr: "127.0.0.1:0"}}

	ln, err := net.Listen("tcp", a.cfg.HealthAddr)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", a.healthz)
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	defer func() { _ = srv.Close() }()

	resp, err := http.Get("http://" + ln.Addr().String() + "/healthz")
	if err != nil {
		t.Fatalf("get /healthz: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if strings.TrimSpace(string(body)) != "ok" {
		t.Errorf("body = %q, want ok", body)
	}
}

// An agent writes Markdown, and a plain Lark text message renders none of it —
// the first real conversation through this adapter arrived as literal asterisks
// and hyphens. A card's lark_md field renders it.
func TestRenderReplyAsCard(t *testing.T) {
	a := &adapter{cfg: &config{Card: true}}
	msgType, content, err := a.renderReply("**bold** and\n- a bullet")
	if err != nil {
		t.Fatalf("renderReply: %v", err)
	}
	if msgType != "interactive" {
		t.Errorf("msgType = %q, want interactive", msgType)
	}

	var card struct {
		Config   map[string]any `json:"config"`
		Elements []struct {
			Tag  string `json:"tag"`
			Text struct {
				Tag     string `json:"tag"`
				Content string `json:"content"`
			} `json:"text"`
		} `json:"elements"`
	}
	if err := json.Unmarshal([]byte(content), &card); err != nil {
		t.Fatalf("card is not valid JSON: %v\n%s", err, content)
	}
	if len(card.Elements) != 1 {
		t.Fatalf("elements = %d, want 1", len(card.Elements))
	}
	// lark_md is the whole point; a plain "text" tag would render nothing.
	if card.Elements[0].Text.Tag != "lark_md" {
		t.Errorf("text tag = %q, want lark_md", card.Elements[0].Text.Tag)
	}
	if !strings.Contains(card.Elements[0].Text.Content, "**bold**") {
		t.Errorf("the answer did not survive into the card: %s", content)
	}
}

func TestSanitizeMarkdownForLarkConvertsHeadings(t *testing.T) {
	got := sanitizeMarkdownForLark("# Title\n\nbody\n## Sub\nmore")
	want := "**Title**\n\nbody\n**Sub**\nmore"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestSanitizeMarkdownForLarkConvertsTables(t *testing.T) {
	in := "before\n" +
		"| Kind | Desc |\n" +
		"|---|---|\n" +
		"| Agent | core resource |\n" +
		"| Model | endpoint |\n" +
		"after"
	got := sanitizeMarkdownForLark(in)
	if strings.Contains(got, "|---|") {
		t.Errorf("table separator leaked through unconverted: %s", got)
	}
	want := "before\n" +
		"- **Kind**\uff1aAgent\uff0c**Desc**\uff1acore resource\n" +
		"- **Kind**\uff1aModel\uff0c**Desc**\uff1aendpoint\n" +
		"after"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestSanitizeMarkdownForLarkLeavesPlainTextAlone(t *testing.T) {
	in := "**bold** and a `code` span, no headings or tables here."
	if got := sanitizeMarkdownForLark(in); got != in {
		t.Errorf("plain markdown should pass through unchanged, got %q", got)
	}
}

// Cards are opt-out, because some deployments prefer plain text.
func TestRenderReplyAsText(t *testing.T) {
	a := &adapter{cfg: &config{Card: false}}
	msgType, content, err := a.renderReply("hello")
	if err != nil {
		t.Fatalf("renderReply: %v", err)
	}
	if msgType != "text" {
		t.Errorf("msgType = %q, want text", msgType)
	}
	var tc textContent
	if err := json.Unmarshal([]byte(content), &tc); err != nil {
		t.Fatalf("content is not valid JSON: %v", err)
	}
	if tc.Text != "hello" {
		t.Errorf("text = %q, want hello", tc.Text)
	}
}

// Cards are on unless spec.config turns them off.
func TestCardDefaultsOn(t *testing.T) {
	dir := writeCredential(t, map[string]string{keyAppID: testAppID, keyAppSecret: testSecret})
	t.Setenv("AGENTPLANE_AGENT_ENDPOINT", testAgentEP)
	t.Setenv("AGENTPLANE_CREDENTIAL_PATH", dir)
	t.Setenv("AGENTPLANE_TRIGGER_CONFIG", "")

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if !cfg.Card {
		t.Error("Card should default to true")
	}

	t.Setenv("AGENTPLANE_TRIGGER_CONFIG", `{"card":false}`)
	cfg, err = loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.Card {
		t.Error("card:false from spec.config was ignored")
	}
}

func TestRenderConfirmationAsCard(t *testing.T) {
	a := &adapter{cfg: &config{Card: true}}
	c := &confirmation{Summary: "push to main", Options: []confirmOption{
		{Label: "同意", Value: "approve"},
		{Label: "拒绝", Value: "reject"},
	}}
	msgType, content, err := a.renderConfirmation(testChatID, "about to push to main", c)
	if err != nil {
		t.Fatalf("renderConfirmation: %v", err)
	}
	if msgType != "interactive" {
		t.Errorf("msgType = %q, want interactive", msgType)
	}

	var card struct {
		Elements []struct {
			Tag     string `json:"tag"`
			Actions []struct {
				Value map[string]any `json:"value"`
			} `json:"actions"`
		} `json:"elements"`
	}
	if err := json.Unmarshal([]byte(content), &card); err != nil {
		t.Fatalf("card is not valid JSON: %v\n%s", err, content)
	}
	var actions []struct {
		Value map[string]any `json:"value"`
	}
	for _, el := range card.Elements {
		if el.Tag == "action" {
			actions = el.Actions
		}
	}
	if len(actions) != 2 {
		t.Fatalf("actions = %d, want 2", len(actions))
	}
	if actions[0].Value["sessionId"] != testChatID || actions[0].Value["value"] != "approve" || actions[0].Value["label"] != "同意" {
		t.Errorf("first button value = %+v, want sessionId/value/label for the approve option", actions[0].Value)
	}
}

func TestRenderConfirmationAsText(t *testing.T) {
	a := &adapter{cfg: &config{Card: false}}
	c := &confirmation{Summary: "push to main", Options: []confirmOption{
		{Label: "同意", Value: "approve"},
		{Label: "拒绝", Value: "reject"},
	}}
	msgType, content, err := a.renderConfirmation(testChatID, "about to push to main", c)
	if err != nil {
		t.Fatalf("renderConfirmation: %v", err)
	}
	if msgType != "text" {
		t.Errorf("msgType = %q, want text", msgType)
	}
	var tc textContent
	if err := json.Unmarshal([]byte(content), &tc); err != nil {
		t.Fatalf("content is not valid JSON: %v", err)
	}
	if !strings.Contains(tc.Text, "同意") || !strings.Contains(tc.Text, "拒绝") {
		t.Errorf("text fallback = %q, want it to name both options", tc.Text)
	}
}

func TestCardActionValueRoundtripsWhatBuildConfirmationCardSet(t *testing.T) {
	raw, err := buildConfirmationCard(testChatID, "about to push", &confirmation{Options: []confirmOption{
		{Label: "同意", Value: "approve"},
	}})
	if err != nil {
		t.Fatalf("buildConfirmationCard: %v", err)
	}

	var card struct {
		Elements []struct {
			Tag     string `json:"tag"`
			Actions []struct {
				Value map[string]any `json:"value"`
			} `json:"actions"`
		} `json:"elements"`
	}
	if err := json.Unmarshal(raw, &card); err != nil {
		t.Fatalf("card is not valid JSON: %v", err)
	}
	var value map[string]any
	for _, el := range card.Elements {
		if el.Tag == "action" {
			value = el.Actions[0].Value
		}
	}

	sessionID, optValue, label := cardActionValue(&callback.CallBackAction{Value: value})
	if sessionID != testChatID || optValue != "approve" || label != "同意" {
		t.Errorf("cardActionValue = (%q, %q, %q), want (%q, %q, %q)", sessionID, optValue, label, testChatID, "approve", "同意")
	}
}

func TestCardActionValueHandlesNilAction(t *testing.T) {
	sessionID, value, label := cardActionValue(nil)
	if sessionID != "" || value != "" || label != "" {
		t.Errorf("cardActionValue(nil) = (%q, %q, %q), want all empty", sessionID, value, label)
	}
}
