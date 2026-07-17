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

var _ = Describe("Tool Webhook", func() {
	var (
		obj       *corev1alpha1.Tool
		oldObj    *corev1alpha1.Tool
		validator ToolCustomValidator
	)

	BeforeEach(func() {
		obj = &corev1alpha1.Tool{}
		oldObj = &corev1alpha1.Tool{}
		validator = ToolCustomValidator{}
		Expect(validator).NotTo(BeNil(), "Expected validator to be initialized")
		Expect(oldObj).NotTo(BeNil(), "Expected oldObj to be initialized")
		Expect(obj).NotTo(BeNil(), "Expected obj to be initialized")
		// TODO (user): Add any setup logic common to all tests
	})

	AfterEach(func() {
		// TODO (user): Add any teardown logic common to all tests
	})

	Context("When creating or updating Tool under Validating Webhook", func() {
		ctx := context.Background()

		It("admits a non-mcp tool without an mcpServerRef", func() {
			obj.Spec.Type = "http"
			Expect(validator.ValidateCreate(ctx, obj)).Error().NotTo(HaveOccurred())
		})

		It("denies an mcp tool that has no mcpServerRef", func() {
			obj.Spec.Type = "mcp"
			obj.Spec.MCPServerRef = nil
			Expect(validator.ValidateCreate(ctx, obj)).Error().To(HaveOccurred())
		})

		It("admits an mcp tool once an mcpServerRef is set", func() {
			obj.Spec.Type = "mcp"
			obj.Spec.MCPServerRef = &corev1alpha1.LocalReference{Name: "orders-mcp"}
			Expect(validator.ValidateUpdate(ctx, oldObj, obj)).Error().NotTo(HaveOccurred())
		})
	})

})
