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
	"fmt"

	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	corev1alpha1 "github.com/hkmdxlftjf/agent-plane/api/v1alpha1"
)

// nolint:unused
// log is for logging in this package.
var toollog = logf.Log.WithName("tool-resource")

// SetupToolWebhookWithManager registers the webhook for Tool in the manager.
func SetupToolWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr).For(&corev1alpha1.Tool{}).
		WithValidator(&ToolCustomValidator{}).
		Complete()
}

// TODO(user): EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!

// TODO(user): change verbs to "verbs=create;update;delete" if you want to enable deletion validation.
// NOTE: The 'path' attribute must follow a specific pattern and should not be modified directly here.
// Modifying the path for an invalid path can cause API server errors; failing to locate the webhook.
// +kubebuilder:webhook:path=/validate-core-cognet-io-v1alpha1-tool,mutating=false,failurePolicy=fail,sideEffects=None,groups=core.cognet.io,resources=tools,verbs=create;update,versions=v1alpha1,name=vtool-v1alpha1.kb.io,admissionReviewVersions=v1

// ToolCustomValidator struct is responsible for validating the Tool resource
// when it is created, updated, or deleted.
//
// NOTE: The +kubebuilder:object:generate=false marker prevents controller-gen from generating DeepCopy methods,
// as this struct is used only for temporary operations and does not need to be deeply copied.
type ToolCustomValidator struct {
	// TODO(user): Add more fields as needed for validation
}

var _ webhook.CustomValidator = &ToolCustomValidator{}

// ValidateCreate implements webhook.CustomValidator so a webhook will be registered for the type Tool.
func (v *ToolCustomValidator) ValidateCreate(_ context.Context, obj runtime.Object) (admission.Warnings, error) {
	tool, ok := obj.(*corev1alpha1.Tool)
	if !ok {
		return nil, fmt.Errorf("expected a Tool object but got %T", obj)
	}
	toollog.Info("Validation for Tool upon creation", "name", tool.GetName())

	return nil, tool.Spec.Validate()
}

// ValidateUpdate implements webhook.CustomValidator so a webhook will be registered for the type Tool.
func (v *ToolCustomValidator) ValidateUpdate(_ context.Context, oldObj, newObj runtime.Object) (admission.Warnings, error) {
	tool, ok := newObj.(*corev1alpha1.Tool)
	if !ok {
		return nil, fmt.Errorf("expected a Tool object for the newObj but got %T", newObj)
	}
	toollog.Info("Validation for Tool upon update", "name", tool.GetName())

	return nil, tool.Spec.Validate()
}

// ValidateDelete implements webhook.CustomValidator so a webhook will be registered for the type Tool.
func (v *ToolCustomValidator) ValidateDelete(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	tool, ok := obj.(*corev1alpha1.Tool)
	if !ok {
		return nil, fmt.Errorf("expected a Tool object but got %T", obj)
	}
	toollog.Info("Validation for Tool upon deletion", "name", tool.GetName())

	// TODO(user): fill in your validation logic upon object deletion.

	return nil, nil
}
