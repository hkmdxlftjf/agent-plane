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
	"context"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/hkmdxlftjf/agent-plane/api/v1alpha1"
)

// discardLog satisfies the server's minimal logging surface.
type discardLog struct{}

func (discardLog) Info(string, ...any)         {}
func (discardLog) Error(error, string, ...any) {}

func testServer(t *testing.T, objs ...runtime.Object) *server {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("add client-go scheme: %v", err)
	}
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add agent-plane scheme: %v", err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(objs...).Build()
	return &server{reader: c, log: discardLog{}}
}

const (
	testNS     = "default"
	toolRefund = "refund"
)

// With no policy referenced the served view must be nil, so a runtime can tell
// "unconstrained" from "constrained by an empty policy".
func TestResolvePolicyNone(t *testing.T) {
	s := testServer(t)
	got := s.resolvePolicy(context.Background(), testNS, corev1alpha1.AgentSpec{})
	if got != nil {
		t.Errorf("resolvePolicy = %+v, want nil", got)
	}
}

// The Registry must ship the merged policy so the runtime can enforce the
// call-time half. Before this, cfg.Policy was always absent and a ToolPolicy
// had no effect anywhere.
func TestResolvePolicyServesMergedView(t *testing.T) {
	cap2 := int32(2)
	s := testServer(t,
		&corev1alpha1.Policy{
			ObjectMeta: metav1.ObjectMeta{Name: "guardrails", Namespace: testNS},
			Spec: corev1alpha1.PolicySpec{
				Models: &corev1alpha1.AccessRule{Allow: []string{"claude"}},
				Tools:  &corev1alpha1.AccessRule{Deny: []string{"delete-account"}},
			},
		},
		&corev1alpha1.ToolPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: "tool-limits", Namespace: testNS},
			Spec: corev1alpha1.ToolPolicySpec{
				Rules: []corev1alpha1.ToolRule{
					{Tool: toolRefund, Action: corev1alpha1.ToolActionAllow, MaxCallsPerSession: &cap2},
					{Tool: "*", Action: corev1alpha1.ToolActionDeny},
				},
				DefaultAction: corev1alpha1.ToolActionDeny,
			},
		},
	)

	got := s.resolvePolicy(context.Background(), testNS, corev1alpha1.AgentSpec{
		PolicyRefs:     []corev1alpha1.LocalReference{{Name: "guardrails"}},
		ToolPolicyRefs: []corev1alpha1.LocalReference{{Name: "tool-limits"}},
	})
	if got == nil {
		t.Fatal("resolvePolicy = nil, want a merged view")
	}
	if strings.Join(got.Sources, ",") != "Policy/guardrails,ToolPolicy/tool-limits" {
		t.Errorf("Sources = %v", got.Sources)
	}
	if got.Models == nil || len(got.Models.Allow) != 1 || got.Models.Allow[0] != "claude" {
		t.Errorf("Models = %+v, want allow=[claude]", got.Models)
	}
	if got.Tools == nil || len(got.Tools.Deny) != 1 || got.Tools.Deny[0] != "delete-account" {
		t.Errorf("Tools = %+v, want deny=[delete-account]", got.Tools)
	}
	if got.DefaultToolAction != string(corev1alpha1.ToolActionDeny) {
		t.Errorf("DefaultToolAction = %q, want deny", got.DefaultToolAction)
	}
	if len(got.ToolRules) != 2 {
		t.Fatalf("ToolRules = %+v, want 2", got.ToolRules)
	}
	// The cap must survive the CR -> wire conversion; it is the whole point of
	// shipping the view, since only the runtime can count calls.
	if got.ToolRules[0].MaxCallsPerSession == nil || *got.ToolRules[0].MaxCallsPerSession != 2 {
		t.Errorf("MaxCallsPerSession = %v, want 2", got.ToolRules[0].MaxCallsPerSession)
	}
	if got.ToolRules[0].Action != string(corev1alpha1.ToolActionAllow) {
		t.Errorf("rule action = %q, want allow", got.ToolRules[0].Action)
	}
}

// A missing Policy already keeps the Agent out of Ready, so the snapshot must
// still build rather than failing and taking the whole config down with it.
func TestResolvePolicyToleratesMissingRefs(t *testing.T) {
	s := testServer(t, &corev1alpha1.Policy{
		ObjectMeta: metav1.ObjectMeta{Name: "present", Namespace: testNS},
		Spec: corev1alpha1.PolicySpec{
			Models: &corev1alpha1.AccessRule{Deny: []string{"gpt-4"}},
		},
	})

	got := s.resolvePolicy(context.Background(), testNS, corev1alpha1.AgentSpec{
		PolicyRefs: []corev1alpha1.LocalReference{{Name: "present"}, {Name: "absent"}},
	})
	if got == nil {
		t.Fatal("resolvePolicy = nil, want the resolvable half")
	}
	if len(got.Sources) != 1 || got.Sources[0] != "Policy/present" {
		t.Errorf("Sources = %v, want only the Policy that exists", got.Sources)
	}
}

// buildConfigFrom must attach the policy view, since that is the field the
// runtime actually reads.
func TestBuildConfigFromAttachesPolicy(t *testing.T) {
	s := testServer(t,
		&corev1alpha1.Model{
			ObjectMeta: metav1.ObjectMeta{Name: "m", Namespace: testNS},
			Spec:       corev1alpha1.ModelSpec{Provider: "anthropic", ModelName: "claude-opus-4-8"},
		},
		&corev1alpha1.ToolPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: "tp", Namespace: testNS},
			Spec: corev1alpha1.ToolPolicySpec{
				Rules: []corev1alpha1.ToolRule{{Tool: toolRefund, Action: corev1alpha1.ToolActionDeny}},
			},
		},
	)
	agent := &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: testNS},
		Spec: corev1alpha1.AgentSpec{
			ModelRef:       &corev1alpha1.LocalReference{Name: "m"},
			ToolPolicyRefs: []corev1alpha1.LocalReference{{Name: "tp"}},
		},
	}

	cfg, err := s.buildConfigFrom(context.Background(), agent)
	if err != nil {
		t.Fatalf("buildConfigFrom: %v", err)
	}
	if cfg.Policy == nil {
		t.Fatal("cfg.Policy = nil; the runtime would enforce nothing")
	}
	if len(cfg.Policy.ToolRules) != 1 || cfg.Policy.ToolRules[0].Tool != toolRefund {
		t.Errorf("ToolRules = %+v", cfg.Policy.ToolRules)
	}
}

// AgentClass defaults must flow into policy resolution too, otherwise a class
// that attaches guardrails to every Agent of its kind would be ignored on the
// data-plane side while the Operator still enforced it.
func TestResolvePolicyHonorsAgentClassDefaults(t *testing.T) {
	s := testServer(t,
		&corev1alpha1.Model{
			ObjectMeta: metav1.ObjectMeta{Name: "m", Namespace: testNS},
			Spec:       corev1alpha1.ModelSpec{Provider: "anthropic", ModelName: "claude-opus-4-8"},
		},
		&corev1alpha1.AgentClass{
			ObjectMeta: metav1.ObjectMeta{Name: "class", Namespace: testNS},
			Spec: corev1alpha1.AgentClassSpec{
				DefaultPolicyRefs:     []corev1alpha1.LocalReference{{Name: "class-policy"}},
				DefaultToolPolicyRefs: []corev1alpha1.LocalReference{{Name: "class-toolpolicy"}},
			},
		},
		&corev1alpha1.Policy{
			ObjectMeta: metav1.ObjectMeta{Name: "class-policy", Namespace: testNS},
			Spec: corev1alpha1.PolicySpec{
				Models: &corev1alpha1.AccessRule{Deny: []string{"gpt-4"}},
			},
		},
		&corev1alpha1.ToolPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: "class-toolpolicy", Namespace: testNS},
			Spec: corev1alpha1.ToolPolicySpec{
				DefaultAction: corev1alpha1.ToolActionDeny,
			},
		},
	)
	agent := &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: testNS},
		Spec: corev1alpha1.AgentSpec{
			AgentClassRef: &corev1alpha1.LocalReference{Name: "class"},
			ModelRef:      &corev1alpha1.LocalReference{Name: "m"},
		},
	}

	cfg, err := s.buildConfigFrom(context.Background(), agent)
	if err != nil {
		t.Fatalf("buildConfigFrom: %v", err)
	}
	if cfg.Policy == nil {
		t.Fatal("cfg.Policy = nil; the class defaults were dropped")
	}
	if cfg.Policy.Models == nil || len(cfg.Policy.Models.Deny) != 1 {
		t.Errorf("Models = %+v, want the class Policy's deny", cfg.Policy.Models)
	}
	if cfg.Policy.DefaultToolAction != string(corev1alpha1.ToolActionDeny) {
		t.Errorf("DefaultToolAction = %q, want the class ToolPolicy's deny", cfg.Policy.DefaultToolAction)
	}
}
