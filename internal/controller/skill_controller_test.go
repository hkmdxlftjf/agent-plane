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
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	corev1alpha1 "github.com/hkmdxlftjf/agent-plane/api/v1alpha1"
)

var _ = Describe("Skill Controller", func() {
	ctx := context.Background()

	// An inline body needs nothing else to exist, so the Skill is usable at once.
	Context("When the body is inline", func() {
		const resourceName = "test-skill-inline"
		skillKey := types.NamespacedName{Name: resourceName, Namespace: nsDefault}

		BeforeEach(func() {
			skill := &corev1alpha1.Skill{
				ObjectMeta: metav1.ObjectMeta{Name: resourceName, Namespace: nsDefault},
				Spec: corev1alpha1.SkillSpec{
					Description: "an inline skill",
					Content:     "# do the thing",
				},
			}
			Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, skill))).To(Succeed())
		})

		AfterEach(func() {
			deleteIfExists(ctx, skillKey, &corev1alpha1.Skill{})
		})

		It("marks the Skill Ready with contentSource inline", func() {
			reconciler := &SkillReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: skillKey})
			Expect(err).NotTo(HaveOccurred())

			skill := &corev1alpha1.Skill{}
			Expect(k8sClient.Get(ctx, skillKey, skill)).To(Succeed())
			Expect(skill.Status.ContentSource).To(Equal("inline"))
			Expect(meta.IsStatusConditionTrue(skill.Status.Conditions, corev1alpha1.ConditionReady)).To(BeTrue())
			Expect(skill.Status.ObservedGeneration).To(Equal(skill.Generation))
		})
	})

	// A ConfigMap-sourced body is only usable once the ConfigMap and key exist, so
	// the Skill must wait rather than claim readiness.
	Context("When the body comes from a ConfigMap", func() {
		const (
			resourceName = "test-skill-configmap"
			cmName       = "test-skill-cm"
			cmKey        = "SKILL.md"
		)
		skillKey := types.NamespacedName{Name: resourceName, Namespace: nsDefault}
		cmObjKey := types.NamespacedName{Name: cmName, Namespace: nsDefault}

		BeforeEach(func() {
			skill := &corev1alpha1.Skill{
				ObjectMeta: metav1.ObjectMeta{Name: resourceName, Namespace: nsDefault},
				Spec: corev1alpha1.SkillSpec{
					Description: "a ConfigMap-sourced skill",
					ContentConfigMapRef: &corev1alpha1.ConfigMapKeyReference{
						Name: cmName, Key: cmKey,
					},
				},
			}
			Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, skill))).To(Succeed())
		})

		AfterEach(func() {
			deleteIfExists(ctx, skillKey, &corev1alpha1.Skill{})
			deleteIfExists(ctx, cmObjKey, &corev1.ConfigMap{})
		})

		It("stays not Ready while the ConfigMap is absent, then converges", func() {
			reconciler := &SkillReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}

			By("reconciling before the ConfigMap exists")
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: skillKey})
			Expect(err).NotTo(HaveOccurred())

			skill := &corev1alpha1.Skill{}
			Expect(k8sClient.Get(ctx, skillKey, skill)).To(Succeed())
			// contentSource records the *intent* even while unresolved, so an
			// operator can see which path is being waited on.
			Expect(skill.Status.ContentSource).To(Equal("configMap"))
			cond := meta.FindStatusCondition(skill.Status.Conditions, corev1alpha1.ConditionReady)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal(corev1alpha1.ReasonReferenceMissing))
			Expect(cond.Message).To(ContainSubstring(cmName))

			By("creating the ConfigMap")
			cm := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Name: cmName, Namespace: nsDefault},
				Data:       map[string]string{cmKey: "# body from the configmap"},
			}
			Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, cm))).To(Succeed())

			_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: skillKey})
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, skillKey, skill)).To(Succeed())
			Expect(meta.IsStatusConditionTrue(skill.Status.Conditions, corev1alpha1.ConditionReady)).To(BeTrue())
		})
	})

	// A present ConfigMap with the wrong key is a distinct failure from a missing
	// ConfigMap, and the message must say which.
	Context("When the ConfigMap exists but lacks the key", func() {
		const (
			resourceName = "test-skill-wrong-key"
			cmName       = "test-skill-wrong-key-cm"
		)
		skillKey := types.NamespacedName{Name: resourceName, Namespace: nsDefault}
		cmObjKey := types.NamespacedName{Name: cmName, Namespace: nsDefault}

		BeforeEach(func() {
			cm := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Name: cmName, Namespace: nsDefault},
				Data:       map[string]string{"OTHER.md": "wrong key"},
			}
			Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, cm))).To(Succeed())

			skill := &corev1alpha1.Skill{
				ObjectMeta: metav1.ObjectMeta{Name: resourceName, Namespace: nsDefault},
				Spec: corev1alpha1.SkillSpec{
					Description: "points at a missing key",
					ContentConfigMapRef: &corev1alpha1.ConfigMapKeyReference{
						Name: cmName, Key: "SKILL.md",
					},
				},
			}
			Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, skill))).To(Succeed())
		})

		AfterEach(func() {
			deleteIfExists(ctx, skillKey, &corev1alpha1.Skill{})
			deleteIfExists(ctx, cmObjKey, &corev1.ConfigMap{})
		})

		It("marks the Skill not Ready naming the key", func() {
			reconciler := &SkillReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: skillKey})
			Expect(err).NotTo(HaveOccurred())

			skill := &corev1alpha1.Skill{}
			Expect(k8sClient.Get(ctx, skillKey, skill)).To(Succeed())
			cond := meta.FindStatusCondition(skill.Status.Conditions, corev1alpha1.ConditionReady)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal(corev1alpha1.ReasonReferenceMissing))
			Expect(cond.Message).To(ContainSubstring("SKILL.md"))
		})
	})

	// Declaring both content sources is contradictory: which body wins would be
	// arbitrary, so the spec is rejected rather than silently resolved.
	Context("When both content sources are set", func() {
		const (
			resourceName = "test-skill-both-sources"
			cmName       = "test-skill-both-cm"
		)
		skillKey := types.NamespacedName{Name: resourceName, Namespace: nsDefault}

		BeforeEach(func() {
			skill := &corev1alpha1.Skill{
				ObjectMeta: metav1.ObjectMeta{Name: resourceName, Namespace: nsDefault},
				Spec: corev1alpha1.SkillSpec{
					Description:         "contradictory",
					Content:             "# inline",
					ContentConfigMapRef: &corev1alpha1.ConfigMapKeyReference{Name: cmName, Key: "SKILL.md"},
				},
			}
			Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, skill))).To(Succeed())
		})

		AfterEach(func() {
			deleteIfExists(ctx, skillKey, &corev1alpha1.Skill{})
		})

		It("marks the Skill not Ready with reason InvalidSpec", func() {
			reconciler := &SkillReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: skillKey})
			Expect(err).NotTo(HaveOccurred())

			skill := &corev1alpha1.Skill{}
			Expect(k8sClient.Get(ctx, skillKey, skill)).To(Succeed())
			cond := meta.FindStatusCondition(skill.Status.Conditions, corev1alpha1.ConditionReady)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal(corev1alpha1.ReasonInvalidSpec))
			// An invalid spec has no resolved source, so the field must be cleared
			// rather than left asserting a body that was never resolved.
			Expect(skill.Status.ContentSource).To(BeEmpty())
		})
	})

	// allowedTools is recorded verbatim for the Registry to serve; the controller
	// does not police it (the Agent controller checks it against the Agent's tools).
	Context("When the Skill declares allowedTools", func() {
		const resourceName = "test-skill-allowed-tools"
		skillKey := types.NamespacedName{Name: resourceName, Namespace: nsDefault}

		BeforeEach(func() {
			skill := &corev1alpha1.Skill{
				ObjectMeta: metav1.ObjectMeta{Name: resourceName, Namespace: nsDefault},
				Spec: corev1alpha1.SkillSpec{
					Description:  "restricts its tools",
					Content:      "# body",
					AllowedTools: []string{"refund", "order-lookup"},
				},
			}
			Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, skill))).To(Succeed())
		})

		AfterEach(func() {
			deleteIfExists(ctx, skillKey, &corev1alpha1.Skill{})
		})

		It("is Ready and preserves the list", func() {
			reconciler := &SkillReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: skillKey})
			Expect(err).NotTo(HaveOccurred())

			skill := &corev1alpha1.Skill{}
			Expect(k8sClient.Get(ctx, skillKey, skill)).To(Succeed())
			Expect(meta.IsStatusConditionTrue(skill.Status.Conditions, corev1alpha1.ConditionReady)).To(BeTrue())
			Expect(skill.Spec.AllowedTools).To(ConsistOf("refund", "order-lookup"))
		})
	})

	// A deleted Skill must not error the reconcile loop.
	Context("When the Skill does not exist", func() {
		It("returns without error", func() {
			reconciler := &SkillReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			_, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: "gone", Namespace: nsDefault},
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})
})
