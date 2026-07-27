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

var _ = Describe("Reference resolution across resource controllers", func() {
	ctx := context.Background()

	Context("ToolSet", func() {
		const setName = "res-toolset"
		const toolName = "res-tool"
		setKey := types.NamespacedName{Name: setName, Namespace: nsDefault}
		toolKey := types.NamespacedName{Name: toolName, Namespace: nsDefault}

		AfterEach(func() {
			for _, obj := range []client.Object{
				&corev1alpha1.ToolSet{ObjectMeta: metav1.ObjectMeta{Name: setName, Namespace: nsDefault}},
				&corev1alpha1.Tool{ObjectMeta: metav1.ObjectMeta{Name: toolName, Namespace: nsDefault}},
			} {
				_ = k8sClient.Delete(ctx, obj)
			}
		})

		It("is Degraded when a member Tool is missing, Ready once it exists", func() {
			set := &corev1alpha1.ToolSet{
				ObjectMeta: metav1.ObjectMeta{Name: setName, Namespace: nsDefault},
				Spec:       corev1alpha1.ToolSetSpec{ToolRefs: []corev1alpha1.LocalReference{{Name: toolName}}},
			}
			Expect(k8sClient.Create(ctx, set)).To(Succeed())

			reconciler := &ToolSetReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: setKey})
			Expect(err).NotTo(HaveOccurred())

			Expect(k8sClient.Get(ctx, setKey, set)).To(Succeed())
			Expect(set.Status.ResolvedTools).To(Equal(0))
			Expect(meta.IsStatusConditionTrue(set.Status.Conditions, corev1alpha1.ConditionReady)).To(BeFalse())

			By("creating the missing Tool and reconciling again")
			tool := &corev1alpha1.Tool{
				ObjectMeta: metav1.ObjectMeta{Name: toolName, Namespace: nsDefault},
				Spec:       corev1alpha1.ToolSpec{Type: toolTypeHTTP},
			}
			Expect(k8sClient.Create(ctx, tool)).To(Succeed())

			_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: setKey})
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, setKey, set)).To(Succeed())
			Expect(set.Status.ResolvedTools).To(Equal(1))
			Expect(meta.IsStatusConditionTrue(set.Status.Conditions, corev1alpha1.ConditionReady)).To(BeTrue())

			_ = toolKey
		})
	})

	Context(kindCredential, func() {
		const credName = "res-cred"
		const secretName = "res-secret"
		credKey := types.NamespacedName{Name: credName, Namespace: nsDefault}

		AfterEach(func() {
			_ = k8sClient.Delete(ctx, &corev1alpha1.Credential{ObjectMeta: metav1.ObjectMeta{Name: credName, Namespace: nsDefault}})
			_ = k8sClient.Delete(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: nsDefault}})
		})

		It("reports SecretFound=false when the Secret key is absent, true once present", func() {
			cred := &corev1alpha1.Credential{
				ObjectMeta: metav1.ObjectMeta{Name: credName, Namespace: nsDefault},
				Spec: corev1alpha1.CredentialSpec{
					SecretRef: corev1alpha1.SecretKeyReference{Name: secretName, Key: secretKeyAPIKey},
				},
			}
			Expect(k8sClient.Create(ctx, cred)).To(Succeed())

			reconciler := &CredentialReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: credKey})
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, credKey, cred)).To(Succeed())
			Expect(cred.Status.SecretFound).NotTo(BeNil())
			Expect(*cred.Status.SecretFound).To(BeFalse())

			By("creating the Secret with the referenced key")
			secret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: nsDefault},
				Data:       map[string][]byte{secretKeyAPIKey: []byte("s3cr3t")},
			}
			Expect(k8sClient.Create(ctx, secret)).To(Succeed())

			_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: credKey})
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, credKey, cred)).To(Succeed())
			Expect(*cred.Status.SecretFound).To(BeTrue())
			Expect(meta.IsStatusConditionTrue(cred.Status.Conditions, corev1alpha1.ConditionReady)).To(BeTrue())
		})
	})
})
