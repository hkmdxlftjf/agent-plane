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
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	corev1alpha1 "github.com/hkmdxlftjf/agent-plane/api/v1alpha1"
)

// Fixture literals shared across the Trigger specs.
const (
	testLarkAdapterImage = "example/lark-adapter:v1"
	testAdapterImage     = "example/adapter:v1"
)

// envOf flattens a container's environment for assertions.
func envOf(c corev1.Container) map[string]string {
	out := make(map[string]string, len(c.Env))
	for _, e := range c.Env {
		out[e.Name] = e.Value
	}
	return out
}

// servedAgent builds an Agent exposing a runtime endpoint, which is what an
// adapter needs in order to have something to POST to.
func servedAgent(name string, port int32) *corev1alpha1.Agent {
	return &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: nsDefault},
		Spec: corev1alpha1.AgentSpec{
			ModelRef: &corev1alpha1.LocalReference{Name: "any-model"},
			Runtime: &corev1alpha1.AgentRuntimeSpec{
				Image: "example/runtime:v1",
				Port:  port,
			},
		},
	}
}

var _ = Describe("Trigger Controller", func() {
	ctx := context.Background()

	Context("When the Agent exposes a runtime endpoint", func() {
		const (
			triggerName = "test-trigger"
			agentName   = "test-trigger-agent"
			credName    = "test-trigger-cred"
			secretName  = "test-trigger-secret"
		)
		triggerKey := types.NamespacedName{Name: triggerName, Namespace: nsDefault}
		depKey := types.NamespacedName{Name: triggerName + "-adapter", Namespace: nsDefault}

		BeforeEach(func() {
			Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, servedAgent(agentName, 8080)))).To(Succeed())

			cred := &corev1alpha1.Credential{
				ObjectMeta: metav1.ObjectMeta{Name: credName, Namespace: nsDefault},
				Spec: corev1alpha1.CredentialSpec{
					SecretRef: corev1alpha1.SecretKeyReference{Name: secretName, Key: "app-secret"},
				},
			}
			Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, cred))).To(Succeed())

			trigger := &corev1alpha1.Trigger{
				ObjectMeta: metav1.ObjectMeta{Name: triggerName, Namespace: nsDefault},
				Spec: corev1alpha1.TriggerSpec{
					AgentRef:      corev1alpha1.LocalReference{Name: agentName},
					Image:         testLarkAdapterImage,
					CredentialRef: &corev1alpha1.LocalReference{Name: credName},
					Config: &runtime.RawExtension{
						Raw: []byte(`{"events":["im.message.receive_v1"]}`),
					},
					Env: []corev1.EnvVar{{Name: "LOG_LEVEL", Value: "debug"}},
				},
			}
			Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, trigger))).To(Succeed())
		})

		AfterEach(func() {
			deleteIfExists(ctx, triggerKey, &corev1alpha1.Trigger{})
			deleteIfExists(ctx, depKey, &appsv1.Deployment{})
			deleteIfExists(ctx, types.NamespacedName{Name: agentName, Namespace: nsDefault}, &corev1alpha1.Agent{})
			deleteIfExists(ctx, types.NamespacedName{Name: credName, Namespace: nsDefault}, &corev1alpha1.Credential{})
		})

		It("materializes an adapter Deployment wired to the contract", func() {
			reconciler := &TriggerReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: triggerKey})
			Expect(err).NotTo(HaveOccurred())

			dep := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, depKey, dep)).To(Succeed())
			Expect(dep.Spec.Template.Spec.Containers).To(HaveLen(1))
			c := dep.Spec.Template.Spec.Containers[0]
			Expect(c.Image).To(Equal(testLarkAdapterImage))

			env := envOf(c)
			// The endpoint is the whole point: the adapter is handed an address and
			// never needs to know how the Agent's runtime is exposed.
			Expect(env["AGENTPLANE_AGENT_ENDPOINT"]).To(Equal("http://" + agentName + "-runtime." + nsDefault + ".svc:8080"))
			Expect(env["AGENTPLANE_AGENT_NAME"]).To(Equal(agentName))
			Expect(env["AGENTPLANE_AGENT_NAMESPACE"]).To(Equal(nsDefault))
			Expect(env["AGENTPLANE_TRIGGER_NAME"]).To(Equal(triggerName))
			// spec.config is passed through verbatim so the control plane stays
			// platform-neutral.
			Expect(env["AGENTPLANE_TRIGGER_CONFIG"]).To(Equal(`{"events":["im.message.receive_v1"]}`))
			// User env survives alongside the injected contract.
			Expect(env["LOG_LEVEL"]).To(Equal("debug"))

			// The credential is mounted, never passed as an env value — the path is
			// injected instead, so the secret does not appear in the pod spec.
			Expect(env["AGENTPLANE_CREDENTIAL_PATH"]).To(Equal(credentialMountPath))
			Expect(c.VolumeMounts).To(HaveLen(1))
			Expect(c.VolumeMounts[0].MountPath).To(Equal(credentialMountPath))
			Expect(c.VolumeMounts[0].ReadOnly).To(BeTrue())
			Expect(dep.Spec.Template.Spec.Volumes).To(HaveLen(1))
			Expect(dep.Spec.Template.Spec.Volumes[0].Secret.SecretName).To(Equal(secretName))
			for _, e := range c.Env {
				Expect(e.Value).NotTo(Equal("app-secret"))
			}

			// Owned, so deleting the Trigger takes the adapter with it.
			Expect(dep.OwnerReferences).To(HaveLen(1))
			Expect(dep.OwnerReferences[0].Kind).To(Equal("Trigger"))
			Expect(*dep.OwnerReferences[0].Controller).To(BeTrue())

			trigger := &corev1alpha1.Trigger{}
			Expect(k8sClient.Get(ctx, triggerKey, trigger)).To(Succeed())
			// No adapter pod is available in envtest, so Pending is correct here:
			// Running would be claiming something the Operator cannot see.
			Expect(trigger.Status.Phase).To(Equal(corev1alpha1.TriggerPhasePending))
			Expect(trigger.Status.AgentEndpoint).To(ContainSubstring(agentName + "-runtime"))
		})

		It("is idempotent across repeated reconciles", func() {
			reconciler := &TriggerReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}

			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: triggerKey})
			Expect(err).NotTo(HaveOccurred())
			first := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, depKey, first)).To(Succeed())

			for range 3 {
				_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: triggerKey})
				Expect(err).NotTo(HaveOccurred())
			}

			after := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, depKey, after)).To(Succeed())
			// Comparing against the first result rather than a magic count: the pod
			// spec must be byte-identical, so env is neither duplicated nor reordered
			// and CreateOrUpdate performs no write after the first.
			Expect(after.Spec.Template.Spec).To(Equal(first.Spec.Template.Spec))
			Expect(after.Generation).To(Equal(first.Generation))
		})
	})

	// An Agent with no runtime exposes nothing to POST to. Injecting an address
	// nothing listens on would surface much later as an adapter that connects to
	// the platform and then silently fails every message.
	Context("When the Agent exposes no endpoint", func() {
		const (
			triggerName = "test-trigger-no-endpoint"
			agentName   = "test-trigger-headless-agent"
		)
		triggerKey := types.NamespacedName{Name: triggerName, Namespace: nsDefault}

		BeforeEach(func() {
			agent := &corev1alpha1.Agent{
				ObjectMeta: metav1.ObjectMeta{Name: agentName, Namespace: nsDefault},
				// No spec.runtime at all.
				Spec: corev1alpha1.AgentSpec{ModelRef: &corev1alpha1.LocalReference{Name: "any-model"}},
			}
			Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, agent))).To(Succeed())

			trigger := &corev1alpha1.Trigger{
				ObjectMeta: metav1.ObjectMeta{Name: triggerName, Namespace: nsDefault},
				Spec: corev1alpha1.TriggerSpec{
					AgentRef: corev1alpha1.LocalReference{Name: agentName},
					Image:    testLarkAdapterImage,
				},
			}
			Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, trigger))).To(Succeed())
		})

		AfterEach(func() {
			deleteIfExists(ctx, triggerKey, &corev1alpha1.Trigger{})
			deleteIfExists(ctx, types.NamespacedName{Name: agentName, Namespace: nsDefault}, &corev1alpha1.Agent{})
			deleteIfExists(ctx, types.NamespacedName{Name: triggerName + "-adapter", Namespace: nsDefault}, &appsv1.Deployment{})
		})

		It("marks the Trigger Degraded and creates no adapter", func() {
			reconciler := &TriggerReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: triggerKey})
			Expect(err).NotTo(HaveOccurred())

			trigger := &corev1alpha1.Trigger{}
			Expect(k8sClient.Get(ctx, triggerKey, trigger)).To(Succeed())
			Expect(trigger.Status.Phase).To(Equal(corev1alpha1.TriggerPhaseDegraded))
			cond := meta.FindStatusCondition(trigger.Status.Conditions, corev1alpha1.ConditionReady)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Message).To(ContainSubstring("spec.runtime"))

			// Nothing should have been scheduled against a dead address.
			dep := &appsv1.Deployment{}
			err = k8sClient.Get(ctx, types.NamespacedName{Name: triggerName + "-adapter", Namespace: nsDefault}, dep)
			Expect(err).To(HaveOccurred())
		})
	})

	// GitOps applies in any order, so a Trigger created before its Agent must
	// converge rather than stay broken.
	Context("When the Agent does not exist yet", func() {
		const (
			triggerName = "test-trigger-waiting"
			agentName   = "test-trigger-late-agent"
		)
		triggerKey := types.NamespacedName{Name: triggerName, Namespace: nsDefault}
		depKey := types.NamespacedName{Name: triggerName + "-adapter", Namespace: nsDefault}

		BeforeEach(func() {
			trigger := &corev1alpha1.Trigger{
				ObjectMeta: metav1.ObjectMeta{Name: triggerName, Namespace: nsDefault},
				Spec: corev1alpha1.TriggerSpec{
					AgentRef: corev1alpha1.LocalReference{Name: agentName},
					Image:    testLarkAdapterImage,
				},
			}
			Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, trigger))).To(Succeed())
		})

		AfterEach(func() {
			deleteIfExists(ctx, triggerKey, &corev1alpha1.Trigger{})
			deleteIfExists(ctx, depKey, &appsv1.Deployment{})
			deleteIfExists(ctx, types.NamespacedName{Name: agentName, Namespace: nsDefault}, &corev1alpha1.Agent{})
		})

		It("is Degraded, then converges once the Agent appears", func() {
			reconciler := &TriggerReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: triggerKey})
			Expect(err).NotTo(HaveOccurred())

			trigger := &corev1alpha1.Trigger{}
			Expect(k8sClient.Get(ctx, triggerKey, trigger)).To(Succeed())
			Expect(trigger.Status.Phase).To(Equal(corev1alpha1.TriggerPhaseDegraded))

			By("creating the Agent")
			Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, servedAgent(agentName, 9000)))).To(Succeed())
			_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: triggerKey})
			Expect(err).NotTo(HaveOccurred())

			Expect(k8sClient.Get(ctx, triggerKey, trigger)).To(Succeed())
			Expect(trigger.Status.Phase).NotTo(Equal(corev1alpha1.TriggerPhaseDegraded))
			dep := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, depKey, dep)).To(Succeed())
			Expect(envOf(dep.Spec.Template.Spec.Containers[0])["AGENTPLANE_AGENT_ENDPOINT"]).To(HaveSuffix(":9000"))
		})
	})

	// Without a credentialRef there must be no mount and no path variable —
	// an adapter that reads AGENTPLANE_CREDENTIAL_PATH unconditionally should
	// fail loudly rather than read an empty directory.
	Context("When no credential is referenced", func() {
		const (
			triggerName = "test-trigger-nocred"
			agentName   = "test-trigger-nocred-agent"
		)
		triggerKey := types.NamespacedName{Name: triggerName, Namespace: nsDefault}
		depKey := types.NamespacedName{Name: triggerName + "-adapter", Namespace: nsDefault}

		BeforeEach(func() {
			Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, servedAgent(agentName, 8080)))).To(Succeed())
			trigger := &corev1alpha1.Trigger{
				ObjectMeta: metav1.ObjectMeta{Name: triggerName, Namespace: nsDefault},
				Spec: corev1alpha1.TriggerSpec{
					AgentRef: corev1alpha1.LocalReference{Name: agentName},
					Image:    testAdapterImage,
				},
			}
			Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, trigger))).To(Succeed())
		})

		AfterEach(func() {
			deleteIfExists(ctx, triggerKey, &corev1alpha1.Trigger{})
			deleteIfExists(ctx, depKey, &appsv1.Deployment{})
			deleteIfExists(ctx, types.NamespacedName{Name: agentName, Namespace: nsDefault}, &corev1alpha1.Agent{})
		})

		It("mounts nothing and injects no credential path", func() {
			reconciler := &TriggerReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: triggerKey})
			Expect(err).NotTo(HaveOccurred())

			dep := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, depKey, dep)).To(Succeed())
			c := dep.Spec.Template.Spec.Containers[0]
			Expect(c.VolumeMounts).To(BeEmpty())
			Expect(dep.Spec.Template.Spec.Volumes).To(BeEmpty())
			Expect(envOf(c)).NotTo(HaveKey("AGENTPLANE_CREDENTIAL_PATH"))
			// Nor should an empty config produce an empty variable.
			Expect(envOf(c)).NotTo(HaveKey("AGENTPLANE_TRIGGER_CONFIG"))
		})
	})

	// A missing Credential must not schedule an adapter that would crash-loop on
	// an absent Secret volume.
	Context("When the referenced Credential is missing", func() {
		const (
			triggerName = "test-trigger-badcred"
			agentName   = "test-trigger-badcred-agent"
		)
		triggerKey := types.NamespacedName{Name: triggerName, Namespace: nsDefault}

		BeforeEach(func() {
			Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, servedAgent(agentName, 8080)))).To(Succeed())
			trigger := &corev1alpha1.Trigger{
				ObjectMeta: metav1.ObjectMeta{Name: triggerName, Namespace: nsDefault},
				Spec: corev1alpha1.TriggerSpec{
					AgentRef:      corev1alpha1.LocalReference{Name: agentName},
					Image:         testAdapterImage,
					CredentialRef: &corev1alpha1.LocalReference{Name: "does-not-exist"},
				},
			}
			Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, trigger))).To(Succeed())
		})

		AfterEach(func() {
			deleteIfExists(ctx, triggerKey, &corev1alpha1.Trigger{})
			deleteIfExists(ctx, types.NamespacedName{Name: agentName, Namespace: nsDefault}, &corev1alpha1.Agent{})
			deleteIfExists(ctx, types.NamespacedName{Name: triggerName + "-adapter", Namespace: nsDefault}, &appsv1.Deployment{})
		})

		It("marks the Trigger Degraded", func() {
			reconciler := &TriggerReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: triggerKey})
			Expect(err).NotTo(HaveOccurred())

			trigger := &corev1alpha1.Trigger{}
			Expect(k8sClient.Get(ctx, triggerKey, trigger)).To(Succeed())
			Expect(trigger.Status.Phase).To(Equal(corev1alpha1.TriggerPhaseDegraded))
			cond := meta.FindStatusCondition(trigger.Status.Conditions, corev1alpha1.ConditionReady)
			Expect(cond.Reason).To(Equal(corev1alpha1.ReasonReferenceMissing))
			Expect(cond.Message).To(ContainSubstring("does-not-exist"))
		})
	})

	Context("When the Trigger does not exist", func() {
		It("returns without error", func() {
			reconciler := &TriggerReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			_, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: "gone", Namespace: nsDefault},
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})
})
