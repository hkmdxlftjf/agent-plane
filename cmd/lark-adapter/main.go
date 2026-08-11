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
//
// When a runtime's response carries a confirmation (docs/adapter-protocol.md's
// additive field), a reply becomes an interactive card with one button per
// option instead of plain text, and a button click (card.action.trigger) feeds
// the chosen option's label back into the same session as an ordinary message
// — see onCardAction. That requires the Lark app console's card callback set
// to the long connection this adapter already holds, a one-time manual
// configuration step this code cannot do for you.
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
	"sync"
	"syscall"
	"time"

	lark "github.com/larksuite/oapi-sdk-go/v3"
	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher/callback"
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
	a := &adapter{cfg: cfg, api: api, http: &http.Client{Timeout: cfg.Timeout}, seen: newDedupe(10 * time.Minute)}

	handler := dispatcher.NewEventDispatcher("", "").
		OnP2MessageReceiveV1(a.onMessage).
		OnP2CardActionTrigger(a.onCardAction)

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
	seen *dedupe
}

// dedupe remembers recently handled message IDs. Lark's long-connection push is
// at-least-once: a reconnect (see the "disconnected"/"trying to reconnect"
// log lines this adapter already prints) can redeliver an event the adapter
// already answered, which without this would show up as the same question
// answered twice in the chat. Message IDs are unique enough, and short-lived
// enough, that a small time-boxed map is all this needs — no persistence
// required, since a redelivery follows the original within seconds, not across
// a pod restart.
type dedupe struct {
	mu  sync.Mutex
	ttl time.Duration
	at  map[string]time.Time
}

func newDedupe(ttl time.Duration) *dedupe {
	return &dedupe{ttl: ttl, at: make(map[string]time.Time)}
}

// seenBefore reports whether id was already handled within the TTL, and
// records it as handled either way. Called once per message, before any work
// starts, so a redelivered event does the least possible before being dropped.
func (d *dedupe) seenBefore(id string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	now := time.Now()
	for k, t := range d.at {
		if now.Sub(t) > d.ttl {
			delete(d.at, k)
		}
	}
	_, ok := d.at[id]
	d.at[id] = now
	return ok
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

	// A redelivered event (see dedupe's doc comment) gets dropped here, before
	// it costs a model call or a second reply in the chat.
	if a.seen.seenBefore(*msg.MessageId) {
		fmt.Printf("= %s duplicate delivery of %s, skipping\n", deref(msg.ChatId), *msg.MessageId)
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

	// Acknowledge receipt before the model runs, not after — see react's doc
	// comment. Run on its own context, not ctx: the goroutine can outlive this
	// function (onMessage returns as soon as a.ask and the reply are done), and
	// ctx is the dispatcher's request context, cancelled once onMessage returns.
	go func() {
		reactCtx, cancel := context.WithTimeout(context.Background(), a.cfg.Timeout)
		defer cancel()
		a.react(reactCtx, *msg.MessageId, "OK")
	}()

	// Rule 2: the platform's conversation id becomes the sessionId, so a chat
	// gets multi-turn memory and two chats never share history. chat_id is stable
	// per conversation and distinct per user in a p2p chat.
	started := time.Now()
	resp, err := a.ask(ctx, chat, text)
	if err != nil {
		fmt.Fprintf(os.Stderr, "✖ %s: %v\n", chat, err)
		a.reply(ctx, *msg.MessageId, "The agent could not answer: "+err.Error())
		return nil
	}
	fmt.Printf("→ %s (%.1fs) %s\n", chat, time.Since(started).Seconds(), truncate(resp.Answer, 120))
	if resp.Confirmation != nil {
		a.replyConfirmation(ctx, *msg.MessageId, chat, resp.Answer, resp.Confirmation)
	} else {
		a.reply(ctx, *msg.MessageId, resp.Answer)
	}
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
// docs/runtime-protocol.md and docs/adapter-protocol.md — the same shape every
// Agent Plane runtime serves.
type chatRequest struct {
	SessionID string `json:"sessionId"`
	Message   string `json:"message"`
}

// confirmOption is one choice offered to the user for a pending confirmation.
// Value is fed back verbatim through docs/adapter-protocol.md's normal
// message path; Label is what a human reads (on a button, or in the fallback
// text prompt).
type confirmOption struct {
	Label string `json:"label"`
	Value string `json:"value"`
}

// confirmation is the optional, additive field a runtime sets on chatResponse
// when the model wants to pause a destructive or uncertain action and hand the
// decision to the user (see cmd/coding-agent's request_confirmation tool).
type confirmation struct {
	Summary string          `json:"summary"`
	Options []confirmOption `json:"options"`
}

type chatResponse struct {
	Answer       string        `json:"answer"`
	Error        string        `json:"error"`
	Confirmation *confirmation `json:"confirmation,omitempty"`
}

// ask performs Rule 1.
func (a *adapter) ask(ctx context.Context, sessionID, message string) (*chatResponse, error) {
	body, err := json.Marshal(chatRequest{SessionID: sessionID, Message: message})
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, a.cfg.Timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(a.cfg.Endpoint, "/")+"/api/chat", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := a.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("runtime returned HTTP %d", resp.StatusCode)
	}
	var out chatResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode runtime response: %w", err)
	}
	// The contract puts runtime failures in the body with HTTP 200, so a status
	// check alone would report success on a failed turn.
	if out.Error != "" {
		return nil, errors.New(out.Error)
	}
	if out.Answer == "" {
		return nil, errors.New("the runtime returned an empty answer")
	}
	return &out, nil
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
				"text": map[string]any{"tag": "lark_md", "content": sanitizeMarkdownForLark(text)},
			},
		},
	}
	return json.Marshal(card)
}

// sanitizeMarkdownForLark rewrites the two GitHub-flavored constructs a coding
// agent's answers actually use that lark_md does not render: '#' headings and
// pipe tables. lark_md shows both as their literal characters (a real answer
// asking "how many CRDs" came back as raw '#'s and '|'s), so this converts
// what it can and leaves everything else — **bold**, lists, `code`, links,
// all genuinely supported — untouched.
func sanitizeMarkdownForLark(text string) string {
	lines := strings.Split(text, "\n")
	out := make([]string, 0, len(lines))
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		if heading, ok := stripHeadingMarker(line); ok {
			out = append(out, "**"+heading+"**")
			continue
		}
		if rows, consumed := parseMarkdownTable(lines[i:]); consumed > 0 {
			out = append(out, rows...)
			i += consumed - 1
			continue
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

func stripHeadingMarker(line string) (heading string, ok bool) {
	trimmed := strings.TrimLeft(line, "#")
	if trimmed == line || !strings.HasPrefix(line, "#") {
		return "", false
	}
	heading = strings.TrimSpace(trimmed)
	if heading == "" {
		return "", false
	}
	return heading, true
}

// parseMarkdownTable turns a header row + '---' separator + data rows,
// starting at lines[0], into a bullet per data row ("- **header**: cell，
// ..."), pairing each cell with its column header so the row stays readable
// as plain text. Returns 0 lines consumed if lines[0] is not a table header.
func parseMarkdownTable(lines []string) (rendered []string, consumed int) {
	if len(lines) < 2 {
		return nil, 0
	}
	headerCells := splitTableRow(lines[0])
	if headerCells == nil || !isTableSeparator(lines[1], len(headerCells)) {
		return nil, 0
	}
	rendered = make([]string, 0, len(lines)-2)
	n := 2
	for ; n < len(lines); n++ {
		cells := splitTableRow(lines[n])
		if cells == nil {
			break
		}
		var row strings.Builder
		row.WriteString("- ")
		for j, cell := range cells {
			if j >= len(headerCells) || cell == "" {
				continue
			}
			if row.Len() > 2 {
				row.WriteString("\uff0c")
			}
			row.WriteString("**" + headerCells[j] + "**\uff1a" + cell)
		}
		rendered = append(rendered, row.String())
	}
	return rendered, n
}

// splitTableRow splits a '| a | b |' line into ["a", "b"], or returns nil if
// line is not a pipe-delimited row at all.
func splitTableRow(line string) []string {
	trimmed := strings.TrimSpace(line)
	if !strings.HasPrefix(trimmed, "|") {
		return nil
	}
	trimmed = strings.Trim(trimmed, "|")
	parts := strings.Split(trimmed, "|")
	cells := make([]string, len(parts))
	for i, p := range parts {
		cells[i] = strings.TrimSpace(p)
	}
	return cells
}

// isTableSeparator reports whether line is a '|---|---|'-style row with
// exactly want columns, each cell containing only '-' and ':'.
func isTableSeparator(line string, want int) bool {
	cells := splitTableRow(line)
	if len(cells) != want {
		return false
	}
	for _, c := range cells {
		if c == "" || strings.Trim(c, "-:") != "" {
			return false
		}
	}
	return true
}

// react adds an emoji reaction to messageID as a receipt acknowledgment,
// fired the moment a message is accepted for handling rather than after the
// model answers — a coding agent's turn can take many seconds, and without
// this the only sign the message arrived is silence until the reply lands.
// Best-effort: a failure here (e.g. the app lacks the reaction scope) is
// logged and never blocks the actual reply.
func (a *adapter) react(ctx context.Context, messageID, emojiType string) {
	req := larkim.NewCreateMessageReactionReqBuilder().
		MessageId(messageID).
		Body(larkim.NewCreateMessageReactionReqBodyBuilder().
			ReactionType(larkim.NewEmojiBuilder().EmojiType(emojiType).Build()).
			Build()).
		Build()

	resp, err := a.api.Im.MessageReaction.Create(ctx, req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "  react to lark: %v\n", err)
		return
	}
	if !resp.Success() {
		fmt.Fprintf(os.Stderr, "  react to lark: code=%d msg=%s\n", resp.Code, resp.Msg)
	}
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
	a.sendReply(ctx, messageID, msgType, content)
}

// replyConfirmation is reply's counterpart for a chatResponse carrying a
// pending confirmation: an interactive card with one button per option when
// cards are enabled, or a plain-text prompt naming the options otherwise —
// either way the user's next message (typed or a button click) flows back
// through the ordinary Rule 1/2 path, because the runtime treats a
// confirmation reply as just another message in the same session.
func (a *adapter) replyConfirmation(ctx context.Context, messageID, sessionID, answer string, c *confirmation) {
	msgType, content, err := a.renderConfirmation(sessionID, answer, c)
	if err != nil {
		fmt.Fprintf(os.Stderr, "  marshal confirmation reply: %v\n", err)
		return
	}
	a.sendReply(ctx, messageID, msgType, content)
}

func (a *adapter) sendReply(ctx context.Context, messageID, msgType, content string) {
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

// push sends a new, unsolicited message into a chat rather than replying to a
// specific one. It is how the follow-up answer after a card click reaches the
// user: card.action.trigger has no message worth replying to (see
// onCardAction), only a chat to speak into.
func (a *adapter) push(ctx context.Context, chatID, text string) {
	msgType, content, err := a.renderReply(text)
	if err != nil {
		fmt.Fprintf(os.Stderr, "  marshal push: %v\n", err)
		return
	}
	a.sendMessage(ctx, chatID, msgType, content)
}

func (a *adapter) pushConfirmation(ctx context.Context, chatID, answer string, c *confirmation) {
	msgType, content, err := a.renderConfirmation(chatID, answer, c)
	if err != nil {
		fmt.Fprintf(os.Stderr, "  marshal confirmation push: %v\n", err)
		return
	}
	a.sendMessage(ctx, chatID, msgType, content)
}

func (a *adapter) sendMessage(ctx context.Context, chatID, msgType, content string) {
	req := larkim.NewCreateMessageReqBuilder().
		ReceiveIdType("chat_id").
		Body(larkim.NewCreateMessageReqBodyBuilder().
			ReceiveId(chatID).
			MsgType(msgType).
			Content(content).
			Build()).
		Build()

	resp, err := a.api.Im.Message.Create(ctx, req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "  push to lark: %v\n", err)
		return
	}
	if !resp.Success() {
		fmt.Fprintf(os.Stderr, "  push to lark: code=%d msg=%s\n", resp.Code, resp.Msg)
	}
}

// onCardAction handles a click on a confirmation card's button.
//
// It requires the Lark app console's "卡片回调" (card callback) set to receive
// events over the long connection — the same one this adapter already holds
// for messages — rather than a webhook URL; that is a one-time app
// configuration step, not something this adapter can set for you.
//
// It must return within Lark's card-callback timeout, which a slow model turn
// (a coding agent may run several bash/read_file/write_file calls) can easily
// exceed. So it acknowledges immediately with an updated card and continues
// the conversation in the background, pushing the eventual answer as a new
// message (push) once ready — the same trade-off a human would make: confirm
// receipt now, report back when done.
func (a *adapter) onCardAction(ctx context.Context, ev *callback.CardActionTriggerEvent) (*callback.CardActionTriggerResponse, error) {
	if ev.Event == nil {
		return nil, nil
	}

	// Same redelivery risk as onMessage (see dedupe's doc comment), and worse
	// here: a redelivered click does not just cost an extra model call, it can
	// race itself updating the same card and surface as an error toast in the
	// client. Token is the card-update credential Lark hands this specific
	// click, unique per click.
	if ev.Event.Token != "" && a.seen.seenBefore(ev.Event.Token) {
		return nil, nil
	}

	sessionID, value, label := cardActionValue(ev.Event.Action)
	if sessionID == "" || label == "" {
		return nil, nil
	}
	fmt.Printf("← %s [card] %s (%s)\n", sessionID, label, value)

	go func() {
		// ask and push get independent budgets, not one shared deadline: a slow
		// model turn must not eat into the time the reply itself needs to reach
		// Lark. ask applies a.cfg.Timeout to context.Background() itself.
		// Rule 2/1: a card click is not a new wire call, it is the next message
		// in the same session — label is exactly what the user would have typed.
		resp, err := a.ask(context.Background(), sessionID, label)

		pushCtx, cancelPush := context.WithTimeout(context.Background(), a.cfg.Timeout)
		defer cancelPush()
		if err != nil {
			fmt.Fprintf(os.Stderr, "✖ %s: %v\n", sessionID, err)
			a.push(pushCtx, sessionID, "The agent could not answer: "+err.Error())
			return
		}
		if resp.Confirmation != nil {
			a.pushConfirmation(pushCtx, sessionID, resp.Answer, resp.Confirmation)
		} else {
			a.push(pushCtx, sessionID, resp.Answer)
		}
	}()

	ack, err := buildAckCard(label)
	if err != nil {
		return nil, err
	}
	return &callback.CardActionTriggerResponse{Card: &callback.Card{Type: "card_json", Data: json.RawMessage(ack)}}, nil
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

// buildConfirmationCard renders a pending confirmation as an interactive card
// with one button per option. Each button's value embeds sessionId, the
// option's value, and its label — onCardAction reads all three back out of
// the click event; Lark echoes whatever a button's value holds verbatim.
func buildConfirmationCard(sessionID, answer string, c *confirmation) ([]byte, error) {
	actions := make([]any, 0, len(c.Options))
	for i, o := range c.Options {
		style := "default"
		if i == 0 {
			style = "primary"
		}
		actions = append(actions, map[string]any{
			"tag":  "button",
			"text": map[string]any{"tag": "plain_text", "content": o.Label},
			"type": style,
			"value": map[string]any{
				"sessionId": sessionID,
				"value":     o.Value,
				"label":     o.Label,
			},
		})
	}
	card := map[string]any{
		"config": map[string]any{"wide_screen_mode": true},
		"elements": []any{
			map[string]any{"tag": "div", "text": map[string]any{"tag": "lark_md", "content": sanitizeMarkdownForLark(answer)}},
			map[string]any{"tag": "action", "actions": actions},
		},
	}
	return json.Marshal(card)
}

// buildAckCard replaces a confirmation card in place once its button has been
// clicked, so the user sees their choice was received rather than a card that
// still looks actionable (or, worse, silently accepts a second click).
func buildAckCard(label string) ([]byte, error) {
	card := map[string]any{
		"config": map[string]any{"wide_screen_mode": true},
		"elements": []any{
			map[string]any{"tag": "div", "text": map[string]any{
				"tag":     "lark_md",
				"content": fmt.Sprintf("✅ 已选择：**%s**，正在处理…", label),
			}},
		},
	}
	return json.Marshal(card)
}

// renderConfirmation is renderReply's counterpart for a pending confirmation:
// a card with real buttons when cards are enabled, or a plain-text prompt
// naming the options otherwise — a text-only chat still gets a usable (if
// less convenient) way to answer, by typing an option's label back.
func (a *adapter) renderConfirmation(sessionID, answer string, c *confirmation) (msgType, content string, err error) {
	if a.cfg.Card {
		raw, cardErr := buildConfirmationCard(sessionID, answer, c)
		if cardErr == nil {
			return larkim.MsgTypeInteractive, string(raw), nil
		}
		fmt.Fprintf(os.Stderr, "  confirmation card render failed, falling back to text: %v\n", cardErr)
	}
	var b strings.Builder
	b.WriteString(answer)
	b.WriteString("\n\n请回复以下选项之一以继续：")
	for _, o := range c.Options {
		fmt.Fprintf(&b, "\n- %s", o.Label)
	}
	raw, err := json.Marshal(textContent{Text: b.String()})
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

// cardActionValue pulls sessionId/value/label back out of a button click's
// action.value, the same map buildConfirmationCard put them into.
func cardActionValue(action *callback.CallBackAction) (sessionID, value, label string) {
	if action == nil {
		return "", "", ""
	}
	sessionID, _ = action.Value["sessionId"].(string)
	value, _ = action.Value["value"].(string)
	label, _ = action.Value["label"].(string)
	return sessionID, value, label
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
