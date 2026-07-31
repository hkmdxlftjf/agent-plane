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

	corev1 "k8s.io/api/core/v1"
)

const envLogLevel = "LOG_LEVEL"

func TestTriggerSpecValidate(t *testing.T) {
	tests := []struct {
		name    string
		spec    TriggerSpec
		wantErr string // substring; empty means valid
	}{
		{
			name: "minimal valid spec",
			spec: TriggerSpec{
				AgentRef: LocalReference{Name: "support-agent"},
				Image:    "example/adapter:v1",
			},
		},
		{
			name:    "agentRef is required",
			spec:    TriggerSpec{Image: "example/adapter:v1"},
			wantErr: "spec.agentRef.name is required",
		},
		{
			name: "user env is allowed alongside the contract",
			spec: TriggerSpec{
				AgentRef: LocalReference{Name: "a"},
				Image:    "i",
				Env:      []corev1.EnvVar{{Name: envLogLevel, Value: "debug"}},
			},
		},
		{
			// Silently overriding this would point the adapter at the wrong Agent —
			// a failure that surfaces as "the bot answers nothing" rather than as an
			// apply-time error.
			name: "reserved endpoint variable is rejected",
			spec: TriggerSpec{
				AgentRef: LocalReference{Name: "a"},
				Image:    "i",
				Env:      []corev1.EnvVar{{Name: EnvAgentEndpoint, Value: "http://evil"}},
			},
			wantErr: EnvAgentEndpoint,
		},
		{
			name: "reserved credential path is rejected",
			spec: TriggerSpec{
				AgentRef: LocalReference{Name: "a"},
				Image:    "i",
				Env:      []corev1.EnvVar{{Name: EnvCredentialPath, Value: "/tmp"}},
			},
			wantErr: EnvCredentialPath,
		},
		{
			name: "duplicate env is rejected",
			spec: TriggerSpec{
				AgentRef: LocalReference{Name: "a"},
				Image:    "i",
				Env: []corev1.EnvVar{
					{Name: envLogLevel, Value: "debug"},
					{Name: envLogLevel, Value: "info"},
				},
			},
			wantErr: "duplicate env var",
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

// Every name the Operator injects must be rejected in spec.env. Walking the
// exported list keeps this honest when a new contract variable is added:
// forgetting to reject it would let a Trigger silently break the contract.
func TestEveryReservedTriggerEnvIsRejected(t *testing.T) {
	if len(ReservedTriggerEnv) == 0 {
		t.Fatal("ReservedTriggerEnv is empty")
	}
	for _, name := range ReservedTriggerEnv {
		t.Run(name, func(t *testing.T) {
			spec := TriggerSpec{
				AgentRef: LocalReference{Name: "a"},
				Image:    "i",
				Env:      []corev1.EnvVar{{Name: name}},
			}
			if err := spec.Validate(); err == nil {
				t.Errorf("Validate() accepted reserved variable %q", name)
			}
		})
	}
}
