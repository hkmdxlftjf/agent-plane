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

package v1alpha1

import (
	"strings"
	"testing"
)

const peerAgentName = "web-agent"

func TestToolSpecValidate(t *testing.T) {
	tests := []struct {
		name    string
		spec    ToolSpec
		wantErr string // substring; empty means valid
	}{
		{
			name: "http tool needs no backing reference",
			spec: ToolSpec{Type: ToolTypeHTTP, Endpoint: "http://example.invalid"},
		},
		{
			name: "mcp tool backed by an MCPServer",
			spec: ToolSpec{Type: ToolTypeMCP, MCPServerRef: &LocalReference{Name: "srv"}},
		},
		{
			// The peer form: consulting another Agent is an ordinary mcp Tool.
			name: "mcp tool backed by a peer Agent",
			spec: ToolSpec{Type: ToolTypeMCP, AgentRef: &LocalReference{Name: peerAgentName}},
		},
		{
			name:    "mcp tool with no backing reference",
			spec:    ToolSpec{Type: ToolTypeMCP},
			wantErr: "requires spec.mcpServerRef or spec.agentRef",
		},
		{
			// Both would be ambiguous about which endpoint wins.
			name: "both references set",
			spec: ToolSpec{
				Type:         ToolTypeMCP,
				MCPServerRef: &LocalReference{Name: "srv"},
				AgentRef:     &LocalReference{Name: peerAgentName},
			},
			wantErr: "only one of",
		},
		{
			name:    "agentRef on a non-mcp tool",
			spec:    ToolSpec{Type: ToolTypeHTTP, AgentRef: &LocalReference{Name: peerAgentName}},
			wantErr: "requires type mcp",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.spec.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Validate() = nil, want an error mentioning %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("Validate() = %q, want it to mention %q", err, tc.wantErr)
			}
		})
	}
}

// The enum admits only the types the platform actually resolves. grpc, wasm,
// plugin, and container were listed before any was implemented, so a Tool naming
// one applied cleanly and failed only at call time. If an implementation lands,
// this test should be updated alongside the enum — not deleted to make room.
func TestOnlyImplementedToolTypesAreDeclared(t *testing.T) {
	implemented := map[ToolType]bool{ToolTypeHTTP: true, ToolTypeMCP: true}
	for _, unimplemented := range []ToolType{"grpc", "wasm", "plugin", "container"} {
		if implemented[unimplemented] {
			t.Errorf("%q is listed as implemented but nothing resolves it", unimplemented)
		}
	}
	if len(implemented) != 2 {
		t.Errorf("implemented tool types = %d; update the CRD enum and the docs together", len(implemented))
	}
}
