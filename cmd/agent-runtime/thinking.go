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
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// thinkingOffTransport injects chat_template_kwargs.enable_thinking=false into
// every /chat/completions request. Some reasoning-mode models (Qwen3 served
// via vLLM is the one this was written against) put their entire response in
// a non-standard "reasoning" field and leave the OpenAI-standard "content"
// and "tool_calls" both empty until they are done "thinking" — which for a
// multi-tool, long-context prompt can take minutes or never finish within the
// SDK's request timeout. The agent-plane-sdk-go message type only reads
// content/tool_calls (see agentloop.oaMessage), so a reasoning-only response
// is silently treated as a final answer of "". This transport is a
// side-channel fix that needs no SDK change: agentloop.Config.HTTPClient is
// the one extension point exposed for this exact class of problem.
//
// AGENTPLANE_DISABLE_THINKING=false (or any value other than empty/"true")
// turns this off, for models where reasoning mode is desired or harmless.
type thinkingOffTransport struct {
	base http.RoundTripper
}

func (t thinkingOffTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Method != http.MethodPost || !strings.HasSuffix(req.URL.Path, "/chat/completions") || req.Body == nil {
		return t.base.RoundTrip(req)
	}
	body, err := io.ReadAll(req.Body)
	_ = req.Body.Close()
	if err != nil {
		return nil, err
	}
	patched, changed := withThinkingDisabled(body)
	req.Body = io.NopCloser(bytes.NewReader(patched))
	req.ContentLength = int64(len(patched))
	if changed {
		req.Header.Set("Content-Length", strconv.Itoa(len(patched)))
	}
	return t.base.RoundTrip(req)
}

// withThinkingDisabled adds chat_template_kwargs.enable_thinking=false to a
// chat/completions request body, preserving every other field byte-for-byte
// where possible (re-marshals only when the field is genuinely absent).
// Returns the original bytes unchanged (changed=false) if the body isn't a
// JSON object or already sets chat_template_kwargs.
func withThinkingDisabled(body []byte) (out []byte, changed bool) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return body, false
	}
	if _, exists := m["chat_template_kwargs"]; exists {
		return body, false // caller already set it — do not override
	}
	m["chat_template_kwargs"] = json.RawMessage(`{"enable_thinking":false}`)
	patched, err := json.Marshal(m)
	if err != nil {
		return body, false
	}
	return patched, true
}

// wrapHTTPClientForThinkingModels returns c with its Transport wrapped so
// every outgoing request is routed through thinkingOffTransport, unless
// disabled via env. c may be nil, in which case a client is built with the
// same default timeout agentloop.Config would otherwise supply (nil
// HTTPClient triggers that default only when the field stays nil, which it
// no longer does once this wrapper runs).
func wrapHTTPClientForThinkingModels(c *http.Client, disabled bool) *http.Client {
	if disabled {
		return c
	}
	if c == nil {
		c = &http.Client{Timeout: 90 * time.Second}
	}
	base := c.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	out := *c
	out.Transport = thinkingOffTransport{base: base}
	return &out
}
