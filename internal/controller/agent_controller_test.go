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

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	corev1alpha1 "github.com/hkmdxlftjf/agent-plane/api/v1alpha1"
)

var _ = Describe("Agent Controller", func() {
	Context("When reconciling an Agent whose references all exist", func() {
		const resourceName = "test-agent"
		const modelName = "test-agent-model"

		ctx := context.Background()
		agentKey := types.NamespacedName{Name: resourceName, Namespace: nsDefault}
		modelKey := types.NamespacedName{Name: modelName, Namespace: nsDefault}

		BeforeEach(func() {
			By("creating the referenced Model")
			model := &corev1alpha1.Model{
				ObjectMeta: metav1.ObjectMeta{Name: modelName, Namespace: nsDefault},
				Spec:       corev1alpha1.ModelSpec{Provider: testProvider, ModelName: testModelName},
			}
			Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, model))).To(Succeed())

			By("creating the Agent referencing the Model")
			agent := &corev1alpha1.Agent{
				ObjectMeta: metav1.ObjectMeta{Name: resourceName, Namespace: nsDefault},
				Spec:       corev1alpha1.AgentSpec{ModelRef: &corev1alpha1.LocalReference{Name: modelName}},
			}
			Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, agent))).To(Succeed())
		})

		AfterEach(func() {
			agent := &corev1alpha1.Agent{}
			if err := k8sClient.Get(ctx, agentKey, agent); err == nil {
				Expect(k8sClient.Delete(ctx, agent)).To(Succeed())
			}
			model := &corev1alpha1.Model{}
			if err := k8sClient.Get(ctx, modelKey, model); err == nil {
				Expect(k8sClient.Delete(ctx, model)).To(Succeed())
			}
		})

		It("marks the Agent Ready with a resolved config hash", func() {
			reconciler := &AgentReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: agentKey})
			Expect(err).NotTo(HaveOccurred())

			agent := &corev1alpha1.Agent{}
			Expect(k8sClient.Get(ctx, agentKey, agent)).To(Succeed())
			Expect(agent.Status.Phase).To(Equal(corev1alpha1.AgentPhaseReady))
			Expect(agent.Status.ResolvedConfigHash).NotTo(BeEmpty())
			Expect(meta.IsStatusConditionTrue(agent.Status.Conditions, corev1alpha1.ConditionReady)).To(BeTrue())
		})
	})

	Context("When reconciling an Agent whose Model is missing", func() {
		const resourceName = "test-agent-missing"

		ctx := context.Background()
		agentKey := types.NamespacedName{Name: resourceName, Namespace: nsDefault}

		BeforeEach(func() {
			agent := &corev1alpha1.Agent{
				ObjectMeta: metav1.ObjectMeta{Name: resourceName, Namespace: nsDefault},
				Spec:       corev1alpha1.AgentSpec{ModelRef: &corev1alpha1.LocalReference{Name: "does-not-exist"}},
			}
			Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, agent))).To(Succeed())
		})

		AfterEach(func() {
			agent := &corev1alpha1.Agent{}
			if err := k8sClient.Get(ctx, agentKey, agent); err == nil {
				Expect(k8sClient.Delete(ctx, agent)).To(Succeed())
			}
		})

		It("marks the Agent Degraded with reason ReferenceNotFound", func() {
			reconciler := &AgentReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: agentKey})
			Expect(err).NotTo(HaveOccurred())

			agent := &corev1alpha1.Agent{}
			Expect(k8sClient.Get(ctx, agentKey, agent)).To(Succeed())
			Expect(agent.Status.Phase).To(Equal(corev1alpha1.AgentPhaseDegraded))
			Expect(agent.Status.ResolvedConfigHash).To(BeEmpty())
			cond := meta.FindStatusCondition(agent.Status.Conditions, corev1alpha1.ConditionReady)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Reason).To(Equal(corev1alpha1.ReasonReferenceMissing))
		})
	})

	// A Policy that forbids what the Agent declares must stop the Agent from
	// going Ready. Without this the Registry would happily serve a config for an
	// Agent the policy was meant to prevent from running at all.
	Context("When an Agent references a Model its Policy denies", func() {
		const (
			resourceName = "test-agent-policy-denied"
			modelName    = "test-agent-policy-model"
			policyName   = "test-agent-policy-deny-model"
		)

		ctx := context.Background()
		agentKey := types.NamespacedName{Name: resourceName, Namespace: nsDefault}

		BeforeEach(func() {
			model := &corev1alpha1.Model{
				ObjectMeta: metav1.ObjectMeta{Name: modelName, Namespace: nsDefault},
				Spec:       corev1alpha1.ModelSpec{Provider: testProvider, ModelName: testModelName},
			}
			Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, model))).To(Succeed())

			pol := &corev1alpha1.Policy{
				ObjectMeta: metav1.ObjectMeta{Name: policyName, Namespace: nsDefault},
				Spec: corev1alpha1.PolicySpec{
					Models: &corev1alpha1.AccessRule{Deny: []string{modelName}},
				},
			}
			Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, pol))).To(Succeed())

			agent := &corev1alpha1.Agent{
				ObjectMeta: metav1.ObjectMeta{Name: resourceName, Namespace: nsDefault},
				Spec: corev1alpha1.AgentSpec{
					ModelRef:   &corev1alpha1.LocalReference{Name: modelName},
					PolicyRefs: []corev1alpha1.LocalReference{{Name: policyName}},
				},
			}
			Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, agent))).To(Succeed())
		})

		AfterEach(func() {
			deleteIfExists(ctx, agentKey, &corev1alpha1.Agent{})
			deleteIfExists(ctx, types.NamespacedName{Name: modelName, Namespace: nsDefault}, &corev1alpha1.Model{})
			deleteIfExists(ctx, types.NamespacedName{Name: policyName, Namespace: nsDefault}, &corev1alpha1.Policy{})
		})

		It("marks the Agent Degraded with reason PolicyViolation", func() {
			reconciler := &AgentReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: agentKey})
			Expect(err).NotTo(HaveOccurred())

			agent := &corev1alpha1.Agent{}
			Expect(k8sClient.Get(ctx, agentKey, agent)).To(Succeed())
			Expect(agent.Status.Phase).To(Equal(corev1alpha1.AgentPhaseDegraded))
			Expect(agent.Status.ResolvedConfigHash).To(BeEmpty())

			ready := meta.FindStatusCondition(agent.Status.Conditions, corev1alpha1.ConditionReady)
			Expect(ready).NotTo(BeNil())
			Expect(ready.Reason).To(Equal(corev1alpha1.ReasonPolicyViolation))
			Expect(ready.Message).To(ContainSubstring(modelName))
			// The message must name the Policy so an operator knows what to edit.
			Expect(ready.Message).To(ContainSubstring(policyName))

			// Resolved stays true: everything exists, it is just not permitted.
			// Keeping the two conditions distinct is what makes the failure legible.
			Expect(meta.IsStatusConditionTrue(agent.Status.Conditions, corev1alpha1.ConditionResolved)).To(BeTrue())
			Expect(meta.IsStatusConditionFalse(agent.Status.Conditions, corev1alpha1.ConditionPolicyCompliant)).To(BeTrue())
		})
	})

	// The mirror case: a Policy that permits what the Agent declares must let it
	// through, and say so, so a green condition is not just the absence of a check.
	Context("When an Agent satisfies its Policy and ToolPolicy", func() {
		const (
			resourceName   = "test-agent-policy-allowed"
			modelName      = "test-agent-allowed-model"
			toolName       = "test-agent-allowed-tool"
			policyName     = "test-agent-policy-allow"
			toolPolicyName = "test-agent-toolpolicy-allow"
		)

		ctx := context.Background()
		agentKey := types.NamespacedName{Name: resourceName, Namespace: nsDefault}
		cap1 := int32(1)

		BeforeEach(func() {
			model := &corev1alpha1.Model{
				ObjectMeta: metav1.ObjectMeta{Name: modelName, Namespace: nsDefault},
				Spec:       corev1alpha1.ModelSpec{Provider: testProvider, ModelName: testModelName},
			}
			Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, model))).To(Succeed())

			tool := &corev1alpha1.Tool{
				ObjectMeta: metav1.ObjectMeta{Name: toolName, Namespace: nsDefault},
				Spec:       corev1alpha1.ToolSpec{Type: toolTypeHTTP, Endpoint: testToolEndpoint},
			}
			Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, tool))).To(Succeed())

			pol := &corev1alpha1.Policy{
				ObjectMeta: metav1.ObjectMeta{Name: policyName, Namespace: nsDefault},
				Spec: corev1alpha1.PolicySpec{
					Models: &corev1alpha1.AccessRule{Allow: []string{modelName}},
					Tools:  &corev1alpha1.AccessRule{Allow: []string{toolName}},
				},
			}
			Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, pol))).To(Succeed())

			// A per-session cap is a call-time concern, so it must NOT block Ready.
			tp := &corev1alpha1.ToolPolicy{
				ObjectMeta: metav1.ObjectMeta{Name: toolPolicyName, Namespace: nsDefault},
				Spec: corev1alpha1.ToolPolicySpec{
					Rules: []corev1alpha1.ToolRule{
						{Tool: toolName, Action: corev1alpha1.ToolActionAllow, MaxCallsPerSession: &cap1},
					},
					DefaultAction: corev1alpha1.ToolActionDeny,
				},
			}
			Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, tp))).To(Succeed())

			agent := &corev1alpha1.Agent{
				ObjectMeta: metav1.ObjectMeta{Name: resourceName, Namespace: nsDefault},
				Spec: corev1alpha1.AgentSpec{
					ModelRef:       &corev1alpha1.LocalReference{Name: modelName},
					ToolRefs:       []corev1alpha1.LocalReference{{Name: toolName}},
					PolicyRefs:     []corev1alpha1.LocalReference{{Name: policyName}},
					ToolPolicyRefs: []corev1alpha1.LocalReference{{Name: toolPolicyName}},
				},
			}
			Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, agent))).To(Succeed())
		})

		AfterEach(func() {
			deleteIfExists(ctx, agentKey, &corev1alpha1.Agent{})
			deleteIfExists(ctx, types.NamespacedName{Name: modelName, Namespace: nsDefault}, &corev1alpha1.Model{})
			deleteIfExists(ctx, types.NamespacedName{Name: toolName, Namespace: nsDefault}, &corev1alpha1.Tool{})
			deleteIfExists(ctx, types.NamespacedName{Name: policyName, Namespace: nsDefault}, &corev1alpha1.Policy{})
			deleteIfExists(ctx, types.NamespacedName{Name: toolPolicyName, Namespace: nsDefault}, &corev1alpha1.ToolPolicy{})
		})

		It("marks the Agent Ready and PolicyCompliant", func() {
			reconciler := &AgentReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: agentKey})
			Expect(err).NotTo(HaveOccurred())

			agent := &corev1alpha1.Agent{}
			Expect(k8sClient.Get(ctx, agentKey, agent)).To(Succeed())
			Expect(agent.Status.Phase).To(Equal(corev1alpha1.AgentPhaseReady))
			Expect(agent.Status.ResolvedConfigHash).NotTo(BeEmpty())

			compliant := meta.FindStatusCondition(agent.Status.Conditions, corev1alpha1.ConditionPolicyCompliant)
			Expect(compliant).NotTo(BeNil())
			Expect(compliant.Status).To(Equal(metav1.ConditionTrue))
			// Naming the sources distinguishes "checked and passed" from
			// "no policies were in play".
			Expect(compliant.Message).To(ContainSubstring(policyName))
			Expect(compliant.Message).To(ContainSubstring(toolPolicyName))
		})
	})

	// A ToolPolicy denying a tool the Agent declares is a declaration-time error:
	// the Agent would advertise a tool it may never call.
	Context("When a ToolPolicy denies a tool the Agent declares", func() {
		const (
			resourceName   = "test-agent-toolpolicy-denied"
			modelName      = "test-agent-tp-denied-model"
			toolName       = "test-agent-tp-denied-tool"
			toolPolicyName = "test-agent-toolpolicy-deny"
		)

		ctx := context.Background()
		agentKey := types.NamespacedName{Name: resourceName, Namespace: nsDefault}

		BeforeEach(func() {
			model := &corev1alpha1.Model{
				ObjectMeta: metav1.ObjectMeta{Name: modelName, Namespace: nsDefault},
				Spec:       corev1alpha1.ModelSpec{Provider: testProvider, ModelName: testModelName},
			}
			Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, model))).To(Succeed())

			tool := &corev1alpha1.Tool{
				ObjectMeta: metav1.ObjectMeta{Name: toolName, Namespace: nsDefault},
				Spec:       corev1alpha1.ToolSpec{Type: toolTypeHTTP, Endpoint: testToolEndpoint},
			}
			Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, tool))).To(Succeed())

			tp := &corev1alpha1.ToolPolicy{
				ObjectMeta: metav1.ObjectMeta{Name: toolPolicyName, Namespace: nsDefault},
				Spec: corev1alpha1.ToolPolicySpec{
					Rules: []corev1alpha1.ToolRule{{Tool: toolName, Action: corev1alpha1.ToolActionDeny}},
				},
			}
			Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, tp))).To(Succeed())

			agent := &corev1alpha1.Agent{
				ObjectMeta: metav1.ObjectMeta{Name: resourceName, Namespace: nsDefault},
				Spec: corev1alpha1.AgentSpec{
					ModelRef:       &corev1alpha1.LocalReference{Name: modelName},
					ToolRefs:       []corev1alpha1.LocalReference{{Name: toolName}},
					ToolPolicyRefs: []corev1alpha1.LocalReference{{Name: toolPolicyName}},
				},
			}
			Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, agent))).To(Succeed())
		})

		AfterEach(func() {
			deleteIfExists(ctx, agentKey, &corev1alpha1.Agent{})
			deleteIfExists(ctx, types.NamespacedName{Name: modelName, Namespace: nsDefault}, &corev1alpha1.Model{})
			deleteIfExists(ctx, types.NamespacedName{Name: toolName, Namespace: nsDefault}, &corev1alpha1.Tool{})
			deleteIfExists(ctx, types.NamespacedName{Name: toolPolicyName, Namespace: nsDefault}, &corev1alpha1.ToolPolicy{})
		})

		It("marks the Agent Degraded with reason PolicyViolation", func() {
			reconciler := &AgentReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: agentKey})
			Expect(err).NotTo(HaveOccurred())

			agent := &corev1alpha1.Agent{}
			Expect(k8sClient.Get(ctx, agentKey, agent)).To(Succeed())
			Expect(agent.Status.Phase).To(Equal(corev1alpha1.AgentPhaseDegraded))
			ready := meta.FindStatusCondition(agent.Status.Conditions, corev1alpha1.ConditionReady)
			Expect(ready).NotTo(BeNil())
			Expect(ready.Reason).To(Equal(corev1alpha1.ReasonPolicyViolation))
			Expect(ready.Message).To(ContainSubstring(toolName))
		})
	})

	// A tool reached through a ToolSet must be policed like a direct toolRef,
	// otherwise a ToolSet is a trivial way to smuggle a denied tool past policy.
	Context("When a denied tool is reached indirectly through a ToolSet", func() {
		const (
			resourceName = "test-agent-toolset-policy"
			modelName    = "test-agent-ts-model"
			toolName     = "test-agent-ts-tool"
			toolSetName  = "test-agent-ts-set"
			policyName   = "test-agent-ts-policy"
		)

		ctx := context.Background()
		agentKey := types.NamespacedName{Name: resourceName, Namespace: nsDefault}

		BeforeEach(func() {
			model := &corev1alpha1.Model{
				ObjectMeta: metav1.ObjectMeta{Name: modelName, Namespace: nsDefault},
				Spec:       corev1alpha1.ModelSpec{Provider: testProvider, ModelName: testModelName},
			}
			Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, model))).To(Succeed())

			tool := &corev1alpha1.Tool{
				ObjectMeta: metav1.ObjectMeta{Name: toolName, Namespace: nsDefault},
				Spec:       corev1alpha1.ToolSpec{Type: toolTypeHTTP, Endpoint: testToolEndpoint},
			}
			Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, tool))).To(Succeed())

			ts := &corev1alpha1.ToolSet{
				ObjectMeta: metav1.ObjectMeta{Name: toolSetName, Namespace: nsDefault},
				Spec: corev1alpha1.ToolSetSpec{
					ToolRefs: []corev1alpha1.LocalReference{{Name: toolName}},
				},
			}
			Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, ts))).To(Succeed())

			pol := &corev1alpha1.Policy{
				ObjectMeta: metav1.ObjectMeta{Name: policyName, Namespace: nsDefault},
				Spec: corev1alpha1.PolicySpec{
					Tools: &corev1alpha1.AccessRule{Deny: []string{toolName}},
				},
			}
			Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, pol))).To(Succeed())

			agent := &corev1alpha1.Agent{
				ObjectMeta: metav1.ObjectMeta{Name: resourceName, Namespace: nsDefault},
				Spec: corev1alpha1.AgentSpec{
					ModelRef:    &corev1alpha1.LocalReference{Name: modelName},
					ToolSetRefs: []corev1alpha1.LocalReference{{Name: toolSetName}},
					PolicyRefs:  []corev1alpha1.LocalReference{{Name: policyName}},
				},
			}
			Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, agent))).To(Succeed())
		})

		AfterEach(func() {
			deleteIfExists(ctx, agentKey, &corev1alpha1.Agent{})
			deleteIfExists(ctx, types.NamespacedName{Name: modelName, Namespace: nsDefault}, &corev1alpha1.Model{})
			deleteIfExists(ctx, types.NamespacedName{Name: toolName, Namespace: nsDefault}, &corev1alpha1.Tool{})
			deleteIfExists(ctx, types.NamespacedName{Name: toolSetName, Namespace: nsDefault}, &corev1alpha1.ToolSet{})
			deleteIfExists(ctx, types.NamespacedName{Name: policyName, Namespace: nsDefault}, &corev1alpha1.Policy{})
		})

		It("still marks the Agent Degraded with reason PolicyViolation", func() {
			reconciler := &AgentReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: agentKey})
			Expect(err).NotTo(HaveOccurred())

			agent := &corev1alpha1.Agent{}
			Expect(k8sClient.Get(ctx, agentKey, agent)).To(Succeed())
			Expect(agent.Status.Phase).To(Equal(corev1alpha1.AgentPhaseDegraded))
			ready := meta.FindStatusCondition(agent.Status.Conditions, corev1alpha1.ConditionReady)
			Expect(ready).NotTo(BeNil())
			Expect(ready.Reason).To(Equal(corev1alpha1.ReasonPolicyViolation))
			Expect(ready.Message).To(ContainSubstring(toolName))
		})
	})
})

// deleteIfExists removes a test fixture if it is still present, keeping the
// AfterEach blocks above short.
func deleteIfExists(ctx context.Context, key types.NamespacedName, obj client.Object) {
	if err := k8sClient.Get(ctx, key, obj); err == nil {
		Expect(k8sClient.Delete(ctx, obj)).To(Succeed())
	}
}
