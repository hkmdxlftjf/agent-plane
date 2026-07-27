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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	corev1alpha1 "github.com/hkmdxlftjf/agent-plane/api/v1alpha1"
)

var _ = Describe("MCPServer Controller", func() {
	Context("When reconciling an MCPServer", func() {
		const resourceName = "test-mcp"

		ctx := context.Background()
		key := types.NamespacedName{Name: resourceName, Namespace: nsDefault}

		BeforeEach(func() {
			mcp := &corev1alpha1.MCPServer{
				ObjectMeta: metav1.ObjectMeta{Name: resourceName, Namespace: nsDefault},
				Spec: corev1alpha1.MCPServerSpec{
					Image:    "ghcr.io/agent-plane/example-mcp:latest",
					Port:     8080,
					Replicas: 1,
				},
			}
			Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, mcp))).To(Succeed())
		})

		AfterEach(func() {
			mcp := &corev1alpha1.MCPServer{}
			if err := k8sClient.Get(ctx, key, mcp); err == nil {
				Expect(k8sClient.Delete(ctx, mcp)).To(Succeed())
			}
		})

		It("creates an owned Deployment and Service and reports an endpoint", func() {
			reconciler := &MCPServerReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())

			By("creating a Deployment owned by the MCPServer")
			dep := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, key, dep)).To(Succeed())
			Expect(dep.Spec.Template.Spec.Containers).To(HaveLen(1))
			Expect(dep.Spec.Template.Spec.Containers[0].Image).To(Equal("ghcr.io/agent-plane/example-mcp:latest"))
			Expect(dep.OwnerReferences).NotTo(BeEmpty())

			By("creating a Service owned by the MCPServer")
			svc := &corev1.Service{}
			Expect(k8sClient.Get(ctx, key, svc)).To(Succeed())
			Expect(svc.Spec.Ports).To(HaveLen(1))
			Expect(svc.Spec.Ports[0].Port).To(Equal(int32(8080)))

			By("reporting an endpoint in status")
			mcp := &corev1alpha1.MCPServer{}
			Expect(k8sClient.Get(ctx, key, mcp)).To(Succeed())
			Expect(mcp.Status.Endpoint).NotTo(BeEmpty())
		})
	})

	Context("When reconciling an external MCPServer", func() {
		const resourceName = "test-mcp-external"

		ctx := context.Background()
		key := types.NamespacedName{Name: resourceName, Namespace: nsDefault}

		BeforeEach(func() {
			mcp := &corev1alpha1.MCPServer{
				ObjectMeta: metav1.ObjectMeta{Name: resourceName, Namespace: nsDefault},
				Spec: corev1alpha1.MCPServerSpec{
					ExternalEndpoint: "https://mcp.example.com/mcp?key=test",
				},
			}
			Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, mcp))).To(Succeed())
		})

		AfterEach(func() {
			mcp := &corev1alpha1.MCPServer{}
			if err := k8sClient.Get(ctx, key, mcp); err == nil {
				Expect(k8sClient.Delete(ctx, mcp)).To(Succeed())
			}
		})

		It("publishes the external URL as the endpoint without creating workloads", func() {
			reconciler := &MCPServerReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())

			By("not creating a Deployment or Service")
			Expect(k8sClient.Get(ctx, key, &appsv1.Deployment{})).NotTo(Succeed())
			Expect(k8sClient.Get(ctx, key, &corev1.Service{})).NotTo(Succeed())

			By("publishing the external URL and Ready=true")
			mcp := &corev1alpha1.MCPServer{}
			Expect(k8sClient.Get(ctx, key, mcp)).To(Succeed())
			Expect(mcp.Status.Endpoint).To(Equal("https://mcp.example.com/mcp?key=test"))
			cond := metav1.Condition{}
			for _, c := range mcp.Status.Conditions {
				if c.Type == corev1alpha1.ConditionReady {
					cond = c
				}
			}
			Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		})

		It("rejects a spec with both image and externalEndpoint", func() {
			bad := &corev1alpha1.MCPServer{
				ObjectMeta: metav1.ObjectMeta{Name: "test-mcp-both", Namespace: nsDefault},
				Spec: corev1alpha1.MCPServerSpec{
					Image:            "example:latest",
					ExternalEndpoint: "https://mcp.example.com/mcp",
				},
			}
			Expect(k8sClient.Create(ctx, bad)).NotTo(Succeed())
		})
	})
})
