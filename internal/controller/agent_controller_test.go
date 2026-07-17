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
		agentKey := types.NamespacedName{Name: resourceName, Namespace: "default"}
		modelKey := types.NamespacedName{Name: modelName, Namespace: "default"}

		BeforeEach(func() {
			By("creating the referenced Model")
			model := &corev1alpha1.Model{
				ObjectMeta: metav1.ObjectMeta{Name: modelName, Namespace: "default"},
				Spec:       corev1alpha1.ModelSpec{Provider: "anthropic", ModelName: "claude-opus-4-8"},
			}
			Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, model))).To(Succeed())

			By("creating the Agent referencing the Model")
			agent := &corev1alpha1.Agent{
				ObjectMeta: metav1.ObjectMeta{Name: resourceName, Namespace: "default"},
				Spec:       corev1alpha1.AgentSpec{ModelRef: corev1alpha1.LocalReference{Name: modelName}},
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
		agentKey := types.NamespacedName{Name: resourceName, Namespace: "default"}

		BeforeEach(func() {
			agent := &corev1alpha1.Agent{
				ObjectMeta: metav1.ObjectMeta{Name: resourceName, Namespace: "default"},
				Spec:       corev1alpha1.AgentSpec{ModelRef: corev1alpha1.LocalReference{Name: "does-not-exist"}},
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
})
