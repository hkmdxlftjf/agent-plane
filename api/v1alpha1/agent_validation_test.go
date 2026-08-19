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

const (
	testRepoURL       = "https://github.com/org/api"
	testVolImage      = "img"
	errUsedByOperator = "is used by the Operator"
)

// An Agent volume that shadows one the Operator manages does not fail as a name
// collision — it fails as an empty checkout or a credential read from the wrong
// file, a long way from the declaration at fault. So every reserved name and
// path is walked here: adding a volume to the controller without reserving it
// should fail this test rather than surface in a cluster.
func TestEveryReservedVolumeIsRejected(t *testing.T) {
	for _, name := range ReservedVolumeNames {
		t.Run("name/"+name, func(t *testing.T) {
			spec := AgentSpec{Runtime: &AgentRuntimeSpec{
				Image: testVolImage,
				Volumes: []AgentVolume{{
					Name:      name,
					MountPath: "/mnt/data",
					EmptyDir:  &AgentEmptyDirVolumeSource{},
				}},
			}}
			err := spec.Validate()
			if err == nil {
				t.Fatalf("Validate() = nil, want the reserved volume name %q to be rejected", name)
			}
			if !strings.Contains(err.Error(), name) {
				t.Errorf("Validate() = %q, want it to name %q", err, name)
			}
		})
	}
	for _, path := range ReservedMountPaths {
		t.Run("path/"+path, func(t *testing.T) {
			spec := AgentSpec{Runtime: &AgentRuntimeSpec{
				Image: testVolImage,
				Volumes: []AgentVolume{{
					Name:      "mine",
					MountPath: path,
					EmptyDir:  &AgentEmptyDirVolumeSource{},
				}},
			}}
			if err := spec.Validate(); err == nil {
				t.Fatalf("Validate() = nil, want the reserved mount path %q to be rejected", path)
			}
		})
	}
}

func TestAgentSpecValidateVolumes(t *testing.T) {
	pvc := func(name, path string, ro bool) AgentVolume {
		return AgentVolume{
			Name: name, MountPath: path, ReadOnly: ro,
			PersistentVolumeClaim: &AgentPVCVolumeSource{ClaimName: "claim"},
		}
	}

	tests := []struct {
		name    string
		spec    AgentSpec
		wantErr string // substring; empty means valid
	}{
		{
			name: "a NAS share alongside a working directory",
			spec: AgentSpec{
				Runtime: &AgentRuntimeSpec{
					Image:   testVolImage,
					Volumes: []AgentVolume{pvc("nas", "/mnt/nas", true)},
				},
				Workspace: &AgentWorkspaceSpec{},
			},
		},
		{
			name: "volumes with no workspace at all",
			spec: AgentSpec{Runtime: &AgentRuntimeSpec{
				Image:   testVolImage,
				Volumes: []AgentVolume{pvc("data", "/mnt/data", false)},
			}},
		},
		{
			name: "shadowing the default workspace mount",
			spec: AgentSpec{
				Runtime: &AgentRuntimeSpec{
					Image:   testVolImage,
					Volumes: []AgentVolume{pvc("mine", "/workspace", false)},
				},
				Workspace: &AgentWorkspaceSpec{},
			},
			wantErr: errUsedByOperator,
		},
		{
			name: "shadowing a relocated workspace mount",
			spec: AgentSpec{
				Runtime: &AgentRuntimeSpec{
					Image:   testVolImage,
					Volumes: []AgentVolume{pvc("mine", "/srv/repo", false)},
				},
				Workspace: &AgentWorkspaceSpec{MountPath: "/srv/repo"},
			},
			wantErr: errUsedByOperator,
		},
		{
			// The path is only the workspace's because that Agent has one. Without
			// a workspace it is an ordinary directory and rejecting it would be
			// superstition.
			name: "the default workspace path is free when there is no workspace",
			spec: AgentSpec{Runtime: &AgentRuntimeSpec{
				Image:   testVolImage,
				Volumes: []AgentVolume{pvc("mine", "/workspace", false)},
			}},
		},
		{
			name: "trailing slash does not evade the check",
			spec: AgentSpec{
				Runtime: &AgentRuntimeSpec{
					Image:   testVolImage,
					Volumes: []AgentVolume{pvc("mine", "/workspace/", false)},
				},
				Workspace: &AgentWorkspaceSpec{},
			},
			wantErr: errUsedByOperator,
		},
		{
			name: "two volumes on one path",
			spec: AgentSpec{Runtime: &AgentRuntimeSpec{
				Image: testVolImage,
				Volumes: []AgentVolume{
					pvc("first", "/mnt/data", false),
					pvc("second", "/mnt/data", false),
				},
			}},
			wantErr: "duplicate mountPath",
		},
		{
			name: "relative mount path",
			spec: AgentSpec{Runtime: &AgentRuntimeSpec{
				Image:   testVolImage,
				Volumes: []AgentVolume{pvc("mine", "mnt/data", false)},
			}},
			wantErr: "must be absolute",
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

// A workspace is a persistent working directory, and a repository is only one
// way to populate it — so the interesting cases are the ones where a field that
// exists solely to describe a clone appears without a clone to describe.
func TestAgentSpecValidateWorkspace(t *testing.T) {
	runtime := &AgentRuntimeSpec{Image: testVolImage}

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
