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
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1alpha1 "github.com/hkmdxlftjf/agent-plane/api/v1alpha1"
)

var _ = Describe("Agent Webhook", func() {
	var (
		obj       *corev1alpha1.Agent
		oldObj    *corev1alpha1.Agent
		validator AgentCustomValidator
	)

	BeforeEach(func() {
		obj = &corev1alpha1.Agent{}
		oldObj = &corev1alpha1.Agent{}
		validator = AgentCustomValidator{}
		Expect(validator).NotTo(BeNil(), "Expected validator to be initialized")
		Expect(oldObj).NotTo(BeNil(), "Expected oldObj to be initialized")
		Expect(obj).NotTo(BeNil(), "Expected obj to be initialized")
		// TODO (user): Add any setup logic common to all tests
	})

	AfterEach(func() {
		// TODO (user): Add any teardown logic common to all tests
	})

	Context("When creating or updating Agent under Validating Webhook", func() {
		ctx := context.Background()

		It("admits an agent with distinct references", func() {
			obj.Spec.ModelRef = corev1alpha1.LocalReference{Name: "m"}
			obj.Spec.ToolRefs = []corev1alpha1.LocalReference{{Name: "a"}, {Name: "b"}}
			Expect(validator.ValidateCreate(ctx, obj)).Error().NotTo(HaveOccurred())
		})

		It("denies an agent with a duplicated toolRef", func() {
			obj.Spec.ModelRef = corev1alpha1.LocalReference{Name: "m"}
			obj.Spec.ToolRefs = []corev1alpha1.LocalReference{{Name: "a"}, {Name: "a"}}
			Expect(validator.ValidateCreate(ctx, obj)).Error().To(HaveOccurred())
		})

		It("denies an agent with a duplicated policyRef on update", func() {
			obj.Spec.ModelRef = corev1alpha1.LocalReference{Name: "m"}
			obj.Spec.PolicyRefs = []corev1alpha1.LocalReference{{Name: "p"}, {Name: "p"}}
			Expect(validator.ValidateUpdate(ctx, oldObj, obj)).Error().To(HaveOccurred())
		})
	})

})
