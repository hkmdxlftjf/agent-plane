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

// Command lark-adapter brings Lark (Feishu) messages into an Agent.
//
// It is the first real implementation of docs/adapter-protocol.md, and it is
// deliberately the whole of the Lark-specific knowledge in this repository: the
// control plane schedules this image and injects the contract, and everything
// about WebSocket subscriptions, message payloads, and reply addressing lives
// here. Adding DingTalk or Slack means another image beside this one, not a
// change to the operator.
//
//	Lark ws event  ──> POST $AGENTPLANE_AGENT_ENDPOINT/api/chat ──> reply via IM API
//
// The long-connection form is what makes this deployable without an ingress:
// the pod dials out to Lark and holds the socket, so no inbound address, no TLS
// certificate, and no webhook URL registration are needed.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	lark "github.com/larksuite/oapi-sdk-go/v3"
	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
	"github.com/larksuite/oapi-sdk-go/v3/ws"
)

// The keys the adapter expects inside the mounted credential Secret. Naming them
// once keeps the error messages, the docs, and the sample from drifting apart.
const (
	keyAppID     = "app-id"
	keyAppSecret = "app-secret"
)

func main() {
	cfg, err := loadConfig()
	if err != nil {
		fatal("configuration", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	api := lark.NewClient(cfg.AppID, cfg.AppSecret)
	a := &adapter{cfg: cfg, api: api, http: &http.Client{Timeout: cfg.Timeout}}

	handler := dispatcher.NewEventDispatcher("", "").
		OnP2MessageReceiveV1(a.onMessage)

	client := ws.NewClient(cfg.AppID, cfg.AppSecret,
		ws.WithEventHandler(handler),
		ws.WithAutoReconnect(true),
		ws.WithLogLevel(larkcore.LogLevelInfo),
	)

	go a.serveHealth(ctx)

	fmt.Printf("▶ lark-adapter for %s/%s → %s\n",
		cfg.AgentNamespace, cfg.AgentName, cfg.Endpoint)

	// Start blocks until the context is cancelled or the connection is
	// unrecoverable. Exiting non-zero is deliberate: a dropped socket that leaves
	// the process alive looks healthy to Kubernetes, so the adapter has to fail
	// loudly enough for the kubelet to restart it (adapter-protocol.md §7).
	if err := client.Start(ctx); err != nil && !errors.Is(err, context.Canceled) {
		fatal("lark connection", err)
	}
}

// config is the contract of docs/adapter-protocol.md §2, resolved once at
// startup so a missing piece fails immediately rather than on the first message.
type config struct {
	Endpoint       string
	AgentName      string
	AgentNamespace string
	TriggerName    string

	AppID     string
	AppSecret string

	// ReplyInThread makes Lark open a thread on the message being answered.
	// Off by default: in a one-on-one chat a thread per answer turns a
	// conversation into a pile of collapsed branches, and the reply is already
	// anchored to its message without one. Turn it on from spec.config for busy
	// group chats, where keeping an answer next to its question is worth it.
	ReplyInThread bool

	// Card sends answers as an interactive card rather than plain text, so the
	// Markdown a model naturally writes actually renders. On by default: a text
	// message shows the asterisks and hyphens verbatim.
	Card       bool
	Timeout    time.Duration
	HealthAddr string
}

// triggerConfig is the shape this adapter reads out of spec.config. The control
// plane passes it through verbatim and interprets nothing.
type triggerConfig struct {
	ReplyInThread *bool `json:"replyInThread,omitempty"`
	Card          *bool `json:"card,omitempty"`
	TimeoutSecond *int  `json:"timeoutSeconds,omitempty"`
}

func loadConfig() (*config, error) {
	cfg := &config{
		Endpoint:       os.Getenv("AGENTPLANE_AGENT_ENDPOINT"),
		AgentName:      os.Getenv("AGENTPLANE_AGENT_NAME"),
		AgentNamespace: os.Getenv("AGENTPLANE_AGENT_NAMESPACE"),
		TriggerName:    os.Getenv("AGENTPLANE_TRIGGER_NAME"),
		ReplyInThread:  false,
		Card:           true,
		// A coding or research agent thinks for a while; the default has to be
		// generous or the adapter gives up mid-answer and the user sees silence.
		Timeout:    5 * time.Minute,
		HealthAddr: ":" + envOr("PORT", "8080"),
	}
	if cfg.Endpoint == "" {
		return nil, errors.New("AGENTPLANE_AGENT_ENDPOINT is empty; the Operator injects it, so this is either not running under a Trigger or the Agent has no runtime port")
	}

	// Credentials arrive as a read-only mount, never as env values, so they stay
	// out of `kubectl describe pod` and the process environment.
	credPath := os.Getenv("AGENTPLANE_CREDENTIAL_PATH")
	if credPath == "" {
		return nil, errors.New("AGENTPLANE_CREDENTIAL_PATH is empty; set spec.credentialRef on the Trigger to a Secret holding app-id and app-secret")
	}
	id, err := readCredential(credPath, keyAppID)
	if err != nil {
		return nil, err
	}
	secret, err := readCredential(credPath, keyAppSecret)
	if err != nil {
		return nil, err
	}
	cfg.AppID, cfg.AppSecret = id, secret

	if raw := os.Getenv("AGENTPLANE_TRIGGER_CONFIG"); raw != "" {
		var tc triggerConfig
		if err := json.Unmarshal([]byte(raw), &tc); err != nil {
			return nil, fmt.Errorf("parse AGENTPLANE_TRIGGER_CONFIG: %w", err)
		}
		if tc.ReplyInThread != nil {
			cfg.ReplyInThread = *tc.ReplyInThread
		}
		if tc.Card != nil {
			cfg.Card = *tc.Card
		}
		if tc.TimeoutSecond != nil && *tc.TimeoutSecond > 0 {
			cfg.Timeout = time.Duration(*tc.TimeoutSecond) * time.Second
		}
	}
	return cfg, nil
}

// readCredential reads one key of the mounted Secret. Naming the missing file in
// the error matters: the usual mistake is a Secret with the right name and the
// wrong keys, which is invisible from the Trigger's status.
func readCredential(dir, key string) (string, error) {
	b, err := os.ReadFile(filepath.Join(dir, key))
	if err != nil {
		return "", fmt.Errorf("read %q from the mounted credential: %w", key, err)
	}
	v := strings.TrimSpace(string(b))
	if v == "" {
		return "", fmt.Errorf("credential key %q is empty", key)
	}
	return v, nil
}

type adapter struct {
	cfg  *config
	api  *lark.Client
	http *http.Client
}

// textContent is Lark's message body for msg_type=text.
type textContent struct {
	Text string `json:"text"`
}

// onMessage is the whole of Rule 1 and Rule 2.
//
// It returns nil even for failures it has reported to the user: returning an
// error makes the SDK treat the event as undelivered, and Lark redelivers — so a
// failing agent would be asked the same question repeatedly. The user already
// has the error in the chat; retrying serves no one.
func (a *adapter) onMessage(ctx context.Context, ev *larkim.P2MessageReceiveV1) error {
	msg := ev.Event.Message
	if msg == nil || msg.ChatId == nil || msg.MessageId == nil {
		return nil
	}

	// Only text is handled. An image or file would otherwise reach the agent as
	// the literal JSON of its payload, which reads as gibberish.
	if deref(msg.MessageType) != larkim.MsgTypeText {
		a.reply(ctx, *msg.MessageId,
			fmt.Sprintf("I can only read text messages; this one is %q.", deref(msg.MessageType)))
		return nil
	}

	text := extractText(deref(msg.Content))
	if text == "" {
		return nil
	}

	// One line per message, both directions. Without it the adapter is silent on
	// the success path, and "did the message even arrive?" — the first question
	// asked when a bot seems unresponsive — has no answer anywhere: Lark shows
	// the message as sent, and the pod looks healthy either way.
	chat := deref(msg.ChatId)
	fmt.Printf("← %s [%s] %s\n", chat, deref(msg.ChatType), truncate(text, 120))

	// Rule 2: the platform's conversation id becomes the sessionId, so a chat
	// gets multi-turn memory and two chats never share history. chat_id is stable
	// per conversation and distinct per user in a p2p chat.
	started := time.Now()
	answer, err := a.ask(ctx, chat, text)
	if err != nil {
		fmt.Fprintf(os.Stderr, "✖ %s: %v\n", chat, err)
		a.reply(ctx, *msg.MessageId, "The agent could not answer: "+err.Error())
		return nil
	}
	fmt.Printf("→ %s (%.1fs) %s\n", chat, time.Since(started).Seconds(), truncate(answer, 120))
	a.reply(ctx, *msg.MessageId, answer)
	return nil
}

// truncate keeps a log line to one line. Message bodies are arbitrary length and
// an untruncated answer would bury every other line in the log.
func truncate(s string, max int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len([]rune(s)) <= max {
		return s
	}
	return string([]rune(s)[:max]) + "…"
}

// extractText pulls the user's words out of Lark's JSON message content. A
// payload that will not parse is skipped rather than forwarded raw, which would
// send the agent a blob of JSON to reason about.
func extractText(content string) string {
	var tc textContent
	if err := json.Unmarshal([]byte(content), &tc); err != nil {
		return ""
	}
	return strings.TrimSpace(tc.Text)
}

// chatRequest and chatResponse are the runtime contract from
// docs/runtime-protocol.md — the same shape every Agent Plane runtime serves.
type chatRequest struct {
	SessionID string `json:"sessionId"`
	Message   string `json:"message"`
}

type chatResponse struct {
	Answer string `json:"answer"`
	Error  string `json:"error"`
}

// ask performs Rule 1.
func (a *adapter) ask(ctx context.Context, sessionID, message string) (string, error) {
	body, err := json.Marshal(chatRequest{SessionID: sessionID, Message: message})
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, a.cfg.Timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(a.cfg.Endpoint, "/")+"/api/chat", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := a.http.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("runtime returned HTTP %d", resp.StatusCode)
	}
	var out chatResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("decode runtime response: %w", err)
	}
	// The contract puts runtime failures in the body with HTTP 200, so a status
	// check alone would report success on a failed turn.
	if out.Error != "" {
		return "", errors.New(out.Error)
	}
	if out.Answer == "" {
		return "", errors.New("the runtime returned an empty answer")
	}
	return out.Answer, nil
}

// buildCard renders an answer as a Lark interactive card.
//
// A plain text message renders nothing: an agent's Markdown — **bold**, bullet
// lists, `code` — arrives as literal asterisks and hyphens, which is how the
// first real conversation through this adapter looked. A card's lark_md field
// renders most of that, so the answer reads the way the model wrote it.
//
// Kept deliberately minimal: one markdown block, no header, no buttons. A card
// heavy with chrome around a one-word answer ("2") is worse than the text was.
func buildCard(text string) ([]byte, error) {
	card := map[string]any{
		// wide_screen_mode lets a long answer use the full width instead of
		// wrapping inside a narrow column.
		"config": map[string]any{"wide_screen_mode": true},
		"elements": []any{
			map[string]any{
				"tag":  "div",
				"text": map[string]any{"tag": "lark_md", "content": text},
			},
		},
	}
	return json.Marshal(card)
}

// reply performs Rule 3. The Agent does not know it is talking to Lark; the
// addressing, and whether the answer becomes a card or plain text, are entirely
// this adapter's business.
func (a *adapter) reply(ctx context.Context, messageID, text string) {
	msgType, content, err := a.renderReply(text)
	if err != nil {
		fmt.Fprintf(os.Stderr, "  marshal reply: %v\n", err)
		return
	}
	req := larkim.NewReplyMessageReqBuilder().
		MessageId(messageID).
		Body(larkim.NewReplyMessageReqBodyBuilder().
			MsgType(msgType).
			Content(content).
			ReplyInThread(a.cfg.ReplyInThread).
			Build()).
		Build()

	resp, err := a.api.Im.Message.Reply(ctx, req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "  reply to lark: %v\n", err)
		return
	}
	if !resp.Success() {
		fmt.Fprintf(os.Stderr, "  reply to lark: code=%d msg=%s\n", resp.Code, resp.Msg)
	}
}

// renderReply picks the message form and serializes it.
//
// Falling back to text on a card-marshal failure matters more than it looks: a
// card that will not serialize would otherwise swallow the answer entirely, and
// the user would see the bot ignore them rather than a plainly formatted reply.
func (a *adapter) renderReply(text string) (msgType, content string, err error) {
	if a.cfg.Card {
		raw, cardErr := buildCard(text)
		if cardErr == nil {
			return larkim.MsgTypeInteractive, string(raw), nil
		}
		fmt.Fprintf(os.Stderr, "  card render failed, falling back to text: %v\n", cardErr)
	}
	raw, err := json.Marshal(textContent{Text: text})
	if err != nil {
		return "", "", err
	}
	return larkim.MsgTypeText, string(raw), nil
}

// healthz answers the readiness probe. It reports only that the process is
// alive: the adapter contract is explicit that Running means "the process
// exists", not that Lark accepted the connection — only the adapter knows the
// latter, and the contract deliberately does not require reporting it back.
func (a *adapter) healthz(w http.ResponseWriter, _ *http.Request) {
	_, _ = w.Write([]byte("ok"))
}

// serveHealth runs the probe endpoint for as long as the process lives.
func (a *adapter) serveHealth(ctx context.Context) {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", a.healthz)
	srv := &http.Server{Addr: a.cfg.HealthAddr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { <-ctx.Done(); _ = srv.Close() }()
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		fmt.Fprintf(os.Stderr, "  health server: %v\n", err)
	}
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func fatal(what string, err error) {
	fmt.Fprintf(os.Stderr, "✖ %s: %v\n", what, err)
	os.Exit(1)
}
