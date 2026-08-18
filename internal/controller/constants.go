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

package controller

// Reference kind names used when resolving cross-resource references.
const (
	kindCredential = "Credential"
	kindTool       = "Tool"
	kindModel      = "Model"
	kindMemory     = "Memory"
)

// Well-known string literals shared across controllers and tests.
const (
	portNameHTTP    = "http"
	toolTypeHTTP    = "http"
	portNameMCP     = "mcp"
	secretKeyAPIKey = "api-key"
	nsDefault       = "default"

	// Standard Kubernetes labels applied to every workload the Operator owns.
	labelName      = "app.kubernetes.io/name"
	labelInstance  = "app.kubernetes.io/instance"
	labelManagedBy = "app.kubernetes.io/managed-by"
	managedByValue = "agent-plane"

	// Skill.status.contentSource values.
	contentSourceInline    = "inline"
	contentSourceConfigMap = "configMap"
)

// Fixture literals shared across the controller tests.
const (
	testProvider     = "anthropic"
	testModelName    = "claude-opus-4-8"
	testToolEndpoint = "http://example.invalid"
	testSkillCMKey   = "SKILL.md"
	testToolSetName  = "bundle"
	testTriggerName  = "lark"
	testAnyModel     = "any-model"
	testMissingName  = "does-not-exist"
	testCodingImage  = "example/coding-agent:v1"
	testRuntimeImage = "example/runtime:v1"
)

// ownedLabels is the label set for a workload the Operator materializes:
// component identifies the kind of workload, instance the owning resource.
func ownedLabels(component, instance string) map[string]string {
	return map[string]string{
		labelName:      component,
		labelInstance:  instance,
		labelManagedBy: managedByValue,
	}
}
