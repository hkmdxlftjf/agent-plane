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
var workflowlog = logf.Log.WithName("workflow-resource")

// SetupWorkflowWebhookWithManager registers the webhook for Workflow in the manager.
func SetupWorkflowWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr).For(&corev1alpha1.Workflow{}).
		WithValidator(&WorkflowCustomValidator{}).
		Complete()
}

// TODO(user): EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!

// TODO(user): change verbs to "verbs=create;update;delete" if you want to enable deletion validation.
// NOTE: The 'path' attribute must follow a specific pattern and should not be modified directly here.
// Modifying the path for an invalid path can cause API server errors; failing to locate the webhook.
// +kubebuilder:webhook:path=/validate-core-hkmdxlftjf-io-v1alpha1-workflow,mutating=false,failurePolicy=fail,sideEffects=None,groups=core.hkmdxlftjf.io,resources=workflows,verbs=create;update,versions=v1alpha1,name=vworkflow-v1alpha1.kb.io,admissionReviewVersions=v1

// WorkflowCustomValidator struct is responsible for validating the Workflow resource
// when it is created, updated, or deleted.
//
// NOTE: The +kubebuilder:object:generate=false marker prevents controller-gen from generating DeepCopy methods,
// as this struct is used only for temporary operations and does not need to be deeply copied.
type WorkflowCustomValidator struct {
	// TODO(user): Add more fields as needed for validation
}

var _ webhook.CustomValidator = &WorkflowCustomValidator{}

// ValidateCreate implements webhook.CustomValidator so a webhook will be registered for the type Workflow.
func (v *WorkflowCustomValidator) ValidateCreate(_ context.Context, obj runtime.Object) (admission.Warnings, error) {
	workflow, ok := obj.(*corev1alpha1.Workflow)
	if !ok {
		return nil, fmt.Errorf("expected a Workflow object but got %T", obj)
	}
	workflowlog.Info("Validation for Workflow upon creation", "name", workflow.GetName())

	return nil, workflow.Spec.Validate()
}

// ValidateUpdate implements webhook.CustomValidator so a webhook will be registered for the type Workflow.
func (v *WorkflowCustomValidator) ValidateUpdate(_ context.Context, oldObj, newObj runtime.Object) (admission.Warnings, error) {
	workflow, ok := newObj.(*corev1alpha1.Workflow)
	if !ok {
		return nil, fmt.Errorf("expected a Workflow object for the newObj but got %T", newObj)
	}
	workflowlog.Info("Validation for Workflow upon update", "name", workflow.GetName())

	return nil, workflow.Spec.Validate()
}

// ValidateDelete implements webhook.CustomValidator so a webhook will be registered for the type Workflow.
func (v *WorkflowCustomValidator) ValidateDelete(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	workflow, ok := obj.(*corev1alpha1.Workflow)
	if !ok {
		return nil, fmt.Errorf("expected a Workflow object but got %T", obj)
	}
	workflowlog.Info("Validation for Workflow upon deletion", "name", workflow.GetName())

	// TODO(user): fill in your validation logic upon object deletion.

	return nil, nil
}
