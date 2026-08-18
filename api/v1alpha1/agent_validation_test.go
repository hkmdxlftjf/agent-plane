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

const testRepoURL = "https://github.com/org/api"

// A workspace is a persistent working directory, and a repository is only one
// way to populate it — so the interesting cases are the ones where a field that
// exists solely to describe a clone appears without a clone to describe.
func TestAgentSpecValidateWorkspace(t *testing.T) {
	runtime := &AgentRuntimeSpec{Image: "img"}

	tests := []struct {
		name    string
		spec    AgentSpec
		wantErr string // substring; empty means valid
	}{
		{
			name: "workspace with a repository",
			spec: AgentSpec{
				Runtime:   runtime,
				Workspace: &AgentWorkspaceSpec{Repository: testRepoURL, Branch: "main"},
			},
		},
		{
			name: "workspace with no repository is a plain persistent directory",
			spec: AgentSpec{
				Runtime:   runtime,
				Workspace: &AgentWorkspaceSpec{Size: "5Gi"},
			},
		},
		{
			name: "branch without a repository",
			spec: AgentSpec{
				Runtime:   runtime,
				Workspace: &AgentWorkspaceSpec{Branch: "main"},
			},
			wantErr: "spec.workspace.branch needs spec.workspace.repository",
		},
		{
			name: "credentialRef without a repository",
			spec: AgentSpec{
				Runtime:   runtime,
				Workspace: &AgentWorkspaceSpec{CredentialRef: &LocalReference{Name: "git-token"}},
			},
			wantErr: "spec.workspace.credentialRef needs spec.workspace.repository",
		},
		{
			name: "workspace without a runtime has no pod to mount into",
			spec: AgentSpec{
				Workspace: &AgentWorkspaceSpec{Repository: testRepoURL},
			},
			wantErr: "spec.workspace requires spec.runtime",
		},
		{
			name: "relative mountPath",
			spec: AgentSpec{
				Runtime:   runtime,
				Workspace: &AgentWorkspaceSpec{MountPath: "workspace"},
			},
			wantErr: "must be absolute",
		},
		{
			name: "unparseable size",
			spec: AgentSpec{
				Runtime:   runtime,
				Workspace: &AgentWorkspaceSpec{Size: "ten gigabytes"},
			},
			wantErr: "is not a valid quantity",
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
