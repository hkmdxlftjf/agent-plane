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
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestWithThinkingDisabledAddsField(t *testing.T) {
	in := []byte(`{"model":"qwen","messages":[{"role":"user","content":"hi"}]}`)
	out, changed := withThinkingDisabled(in)
	if !changed {
		t.Fatal("expected changed=true")
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}
	ctk, ok := m["chat_template_kwargs"].(map[string]any)
	if !ok {
		t.Fatalf("chat_template_kwargs missing or wrong type, got %#v", m["chat_template_kwargs"])
	}
	if ctk["enable_thinking"] != false {
		t.Errorf("enable_thinking = %v, want false", ctk["enable_thinking"])
	}
	if m["model"] != "qwen" {
		t.Error("original fields must survive the patch")
	}
}

func TestWithThinkingDisabledDoesNotOverride(t *testing.T) {
	in := []byte(`{"model":"qwen","chat_template_kwargs":{"enable_thinking":true}}`)
	out, changed := withThinkingDisabled(in)
	if changed {
		t.Fatal("must not touch a request that already sets chat_template_kwargs")
	}
	if string(out) != string(in) {
		t.Error("body must be returned unchanged")
	}
}

func TestWithThinkingDisabledIgnoresNonJSON(t *testing.T) {
	in := []byte("not json")
	out, changed := withThinkingDisabled(in)
	if changed || string(out) != string(in) {
		t.Error("non-JSON body must pass through untouched")
	}
}

func TestThinkingOffTransportOnlyPatchesChatCompletions(t *testing.T) {
	var gotBodies []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBodies = append(gotBodies, string(b))
		w.WriteHeader(200)
	}))
	defer upstream.Close()

	client := wrapHTTPClientForThinkingModels(&http.Client{}, false)

	post := func(path, body string) {
		resp, err := client.Post(upstream.URL+path, "application/json", jsonReader(body))
		if err != nil {
			t.Fatalf("post %s: %v", path, err)
		}
		_ = resp.Body.Close()
	}

	post("/v1/chat/completions", `{"model":"m"}`)
	post("/v1/some/mcp/tool", `{"model":"m"}`)

	if len(gotBodies) != 2 {
		t.Fatalf("expected 2 requests, got %d", len(gotBodies))
	}
	var chatBody map[string]any
	if err := json.Unmarshal([]byte(gotBodies[0]), &chatBody); err != nil {
		t.Fatal(err)
	}
	if chatBody["chat_template_kwargs"] == nil {
		t.Error("chat/completions request must be patched")
	}
	if gotBodies[1] != `{"model":"m"}` {
		t.Errorf("non-chat request must pass through unmodified, got %q", gotBodies[1])
	}
}

func TestWrapHTTPClientDisabled(t *testing.T) {
	orig := &http.Client{}
	got := wrapHTTPClientForThinkingModels(orig, true)
	if got != orig {
		t.Error("disabled=true must return the client unwrapped")
	}
}

func jsonReader(s string) io.Reader {
	return &stringReadCloser{s: s}
}

type stringReadCloser struct {
	s string
	i int
}

func (r *stringReadCloser) Read(p []byte) (int, error) {
	if r.i >= len(r.s) {
		return 0, io.EOF
	}
	n := copy(p, r.s[r.i:])
	r.i += n
	return n, nil
}
