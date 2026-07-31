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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/metrics/server"

	corev1alpha1 "github.com/hkmdxlftjf/agent-plane/api/v1alpha1"
)

// The unit tests exercise the index extractors against a fake client. This one
// registers them on a *real* manager, which is the only place a bad
// registration surfaces — a duplicate (type, field) pair is rejected here, and
// in production a failure to register would leave every indexed watch silently
// matching nothing.
var _ = Describe("Field indexes", func() {
	It("register on a real manager and back a live lookup", func() {
		mgr, err := ctrl.NewManager(cfg, ctrl.Options{
			Scheme:  scheme.Scheme,
			Metrics: server.Options{BindAddress: "0"},
		})
		Expect(err).NotTo(HaveOccurred())

		Expect(SetupFieldIndexes(ctx, mgr)).To(Succeed())

		// Registering the same set twice must fail: proof the manager really
		// took them, rather than SetupFieldIndexes silently doing nothing.
		Expect(SetupFieldIndexes(ctx, mgr)).NotTo(Succeed())

		// Start the cache so a MatchingFields query can actually be served.
		mgrCtx, stop := context.WithCancel(ctx)
		defer stop()
		go func() {
			defer GinkgoRecover()
			_ = mgr.Start(mgrCtx)
		}()
		Expect(mgr.GetCache().WaitForCacheSync(mgrCtx)).To(BeTrue())

		By("creating an Agent that references a Model by name")
		agent := &corev1alpha1.Agent{
			ObjectMeta: metav1.ObjectMeta{Name: "indexed-agent", Namespace: nsDefault},
			Spec: corev1alpha1.AgentSpec{
				ModelRef: &corev1alpha1.LocalReference{Name: "indexed-model"},
			},
		}
		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, agent))).To(Succeed())
		defer deleteIfExists(ctx, types.NamespacedName{Name: "indexed-agent", Namespace: nsDefault}, &corev1alpha1.Agent{})

		By("querying through the index")
		Eventually(func(g Gomega) {
			var agents corev1alpha1.AgentList
			g.Expect(mgr.GetClient().List(mgrCtx, &agents,
				client.InNamespace(nsDefault),
				client.MatchingFields{idxAgentModel: "indexed-model"},
			)).To(Succeed())
			g.Expect(agents.Items).To(HaveLen(1))
			g.Expect(agents.Items[0].Name).To(Equal("indexed-agent"))
		}).Should(Succeed())

		By("confirming an unreferenced name matches nothing")
		var none corev1alpha1.AgentList
		Expect(mgr.GetClient().List(mgrCtx, &none,
			client.InNamespace(nsDefault),
			client.MatchingFields{idxAgentModel: "no-such-model"},
		)).To(Succeed())
		Expect(none.Items).To(BeEmpty())
	})
})
