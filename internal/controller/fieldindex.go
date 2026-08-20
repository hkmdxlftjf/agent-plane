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
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	corev1alpha1 "github.com/hkmdxlftjf/agent-plane/api/v1alpha1"
)

// Field index keys. Each indexes a resource by the *names* it references of one
// dependency kind, so a change to that dependency can be mapped back to exactly
// the resources that care — instead of every resource in the namespace.
//
// The key names are arbitrary but must be unique per (indexed type, field).
const (
	idxAgentModel        = "spec.modelRef"
	idxAgentWorkflow     = "spec.workflowRef"
	idxAgentPrompt       = "spec.promptRef"
	idxAgentTools        = "spec.toolRefs"
	idxAgentToolSets     = "spec.toolSetRefs"
	idxAgentSkills       = "spec.skillRefs"
	idxAgentMemories     = "spec.memoryRefs"
	idxAgentPolicies     = "spec.policyRefs"
	idxAgentToolPolicies = "spec.toolPolicyRefs"
	idxAgentKnowledge    = "spec.knowledgeBaseRefs"
	idxAgentCredentials  = "spec.credentialRefs"
	idxAgentClass        = "spec.agentClassRef"

	idxClassModel        = "spec.defaultModelRef"
	idxClassWorkflow     = "spec.defaultWorkflowRef"
	idxClassPrompt       = "spec.defaultPromptRef"
	idxClassPolicies     = "spec.defaultPolicyRefs"
	idxClassTools        = "spec.defaultToolRefs"
	idxClassSkills       = "spec.defaultSkillRefs"
	idxClassToolPolicies = "spec.defaultToolPolicyRefs"

	idxToolSetTools     = "spec.toolRefs"
	idxToolMCPServer    = "spec.mcpServerRef"
	idxToolAgent        = "spec.agentRef"
	idxModelCredential  = "spec.credentialRef"
	idxMemoryConnection = "spec.connectionRef"
	idxKBModel          = "spec.embeddingModelRef"
	idxKBMemory         = "spec.memoryRef"
	idxKBCredential     = "spec.credentialRef"
	idxSkillConfigMap   = "spec.contentConfigMapRef"
	idxCredentialSecret = "spec.secretRef"

	idxTriggerAgent      = "spec.agentRef"
	idxTriggerCredential = "spec.credentialRef"
)

// refName returns the referenced name, or nil for an unset optional reference —
// indexing an empty string would make every object with the field unset share a
// key.
func refName(ref *corev1alpha1.LocalReference) []string {
	if ref == nil || ref.Name == "" {
		return nil
	}
	return []string{ref.Name}
}

// refNamesOf flattens a reference list into index keys.
func refNamesOf(refs []corev1alpha1.LocalReference) []string {
	if len(refs) == 0 {
		return nil
	}
	out := make([]string, 0, len(refs))
	for _, r := range refs {
		if r.Name != "" {
			out = append(out, r.Name)
		}
	}
	return out
}

// SetupFieldIndexes registers every reference index the controllers rely on.
// It must run before the controllers start, and exactly once per manager —
// controller-runtime rejects a duplicate (type, field) registration.
//
// Registering all of them in one place (rather than each controller indexing
// what it needs) keeps two controllers from racing to register the same index
// and makes the full reverse-reference graph readable at a glance.
func SetupFieldIndexes(ctx context.Context, mgr ctrl.Manager) error {
	idx := mgr.GetFieldIndexer()
	for _, s := range indexSpecs() {
		if err := idx.IndexField(ctx, s.obj, s.field, client.IndexerFunc(s.values)); err != nil {
			return fmt.Errorf("index %T by %s: %w", s.obj, s.field, err)
		}
	}
	return nil
}

// indexSpec is one (type, field, extractor) registration.
type indexSpec struct {
	obj    client.Object
	field  string
	values func(client.Object) []string
}

// indexSpecs is the single declaration of every reference index. Exposing it as
// data lets the tests register the very same extractors into a fake client, so
// what they exercise is what runs in production — a test that re-declared them
// would keep passing after a real extractor started reading the wrong field.
func indexSpecs() []indexSpec {
	return []indexSpec{
		// --- Agent -> its dependencies ---
		{&corev1alpha1.Agent{}, idxAgentModel, func(o client.Object) []string {
			return refName(o.(*corev1alpha1.Agent).Spec.ModelRef)
		}},
		{&corev1alpha1.Agent{}, idxAgentWorkflow, func(o client.Object) []string {
			return refName(o.(*corev1alpha1.Agent).Spec.WorkflowRef)
		}},
		{&corev1alpha1.Agent{}, idxAgentPrompt, func(o client.Object) []string {
			return refName(o.(*corev1alpha1.Agent).Spec.PromptRef)
		}},
		{&corev1alpha1.Agent{}, idxAgentTools, func(o client.Object) []string {
			return refNamesOf(o.(*corev1alpha1.Agent).Spec.ToolRefs)
		}},
		{&corev1alpha1.Agent{}, idxAgentToolSets, func(o client.Object) []string {
			return refNamesOf(o.(*corev1alpha1.Agent).Spec.ToolSetRefs)
		}},
		{&corev1alpha1.Agent{}, idxAgentSkills, func(o client.Object) []string {
			return refNamesOf(o.(*corev1alpha1.Agent).Spec.SkillRefs)
		}},
		{&corev1alpha1.Agent{}, idxAgentMemories, func(o client.Object) []string {
			return refNamesOf(o.(*corev1alpha1.Agent).Spec.MemoryRefs)
		}},
		{&corev1alpha1.Agent{}, idxAgentPolicies, func(o client.Object) []string {
			return refNamesOf(o.(*corev1alpha1.Agent).Spec.PolicyRefs)
		}},
		{&corev1alpha1.Agent{}, idxAgentToolPolicies, func(o client.Object) []string {
			return refNamesOf(o.(*corev1alpha1.Agent).Spec.ToolPolicyRefs)
		}},
		{&corev1alpha1.Agent{}, idxAgentKnowledge, func(o client.Object) []string {
			return refNamesOf(o.(*corev1alpha1.Agent).Spec.KnowledgeBaseRefs)
		}},
		{&corev1alpha1.Agent{}, idxAgentCredentials, func(o client.Object) []string {
			return refNamesOf(o.(*corev1alpha1.Agent).Spec.CredentialRefs)
		}},
		{&corev1alpha1.Agent{}, idxAgentClass, func(o client.Object) []string {
			return refName(o.(*corev1alpha1.Agent).Spec.AgentClassRef)
		}},

		// --- AgentClass -> the defaults it hands down ---
		{&corev1alpha1.AgentClass{}, idxClassModel, func(o client.Object) []string {
			return refName(o.(*corev1alpha1.AgentClass).Spec.DefaultModelRef)
		}},
		{&corev1alpha1.AgentClass{}, idxClassWorkflow, func(o client.Object) []string {
			return refName(o.(*corev1alpha1.AgentClass).Spec.DefaultWorkflowRef)
		}},
		{&corev1alpha1.AgentClass{}, idxClassPrompt, func(o client.Object) []string {
			return refName(o.(*corev1alpha1.AgentClass).Spec.DefaultPromptRef)
		}},
		{&corev1alpha1.AgentClass{}, idxClassPolicies, func(o client.Object) []string {
			return refNamesOf(o.(*corev1alpha1.AgentClass).Spec.DefaultPolicyRefs)
		}},
		{&corev1alpha1.AgentClass{}, idxClassTools, func(o client.Object) []string {
			return refNamesOf(o.(*corev1alpha1.AgentClass).Spec.DefaultToolRefs)
		}},
		{&corev1alpha1.AgentClass{}, idxClassSkills, func(o client.Object) []string {
			return refNamesOf(o.(*corev1alpha1.AgentClass).Spec.DefaultSkillRefs)
		}},
		{&corev1alpha1.AgentClass{}, idxClassToolPolicies, func(o client.Object) []string {
			return refNamesOf(o.(*corev1alpha1.AgentClass).Spec.DefaultToolPolicyRefs)
		}},

		// --- the remaining reference-resolving kinds ---
		{&corev1alpha1.ToolSet{}, idxToolSetTools, func(o client.Object) []string {
			return refNamesOf(o.(*corev1alpha1.ToolSet).Spec.ToolRefs)
		}},
		{&corev1alpha1.Tool{}, idxToolMCPServer, func(o client.Object) []string {
			return refName(o.(*corev1alpha1.Tool).Spec.MCPServerRef)
		}},
		{&corev1alpha1.Tool{}, idxToolAgent, func(o client.Object) []string {
			return refName(o.(*corev1alpha1.Tool).Spec.AgentRef)
		}},
		{&corev1alpha1.Model{}, idxModelCredential, func(o client.Object) []string {
			return refName(o.(*corev1alpha1.Model).Spec.CredentialRef)
		}},
		{&corev1alpha1.Memory{}, idxMemoryConnection, func(o client.Object) []string {
			return refName(o.(*corev1alpha1.Memory).Spec.ConnectionRef)
		}},
		{&corev1alpha1.KnowledgeBase{}, idxKBModel, func(o client.Object) []string {
			return refName(o.(*corev1alpha1.KnowledgeBase).Spec.EmbeddingModelRef)
		}},
		{&corev1alpha1.KnowledgeBase{}, idxKBMemory, func(o client.Object) []string {
			return refName(o.(*corev1alpha1.KnowledgeBase).Spec.MemoryRef)
		}},
		{&corev1alpha1.KnowledgeBase{}, idxKBCredential, func(o client.Object) []string {
			return refName(o.(*corev1alpha1.KnowledgeBase).Spec.CredentialRef)
		}},
		{&corev1alpha1.Skill{}, idxSkillConfigMap, func(o client.Object) []string {
			if ref := o.(*corev1alpha1.Skill).Spec.ContentConfigMapRef; ref != nil && ref.Name != "" {
				return []string{ref.Name}
			}
			return nil
		}},
		{&corev1alpha1.Trigger{}, idxTriggerAgent, func(o client.Object) []string {
			if name := o.(*corev1alpha1.Trigger).Spec.AgentRef.Name; name != "" {
				return []string{name}
			}
			return nil
		}},
		{&corev1alpha1.Trigger{}, idxTriggerCredential, func(o client.Object) []string {
			return refName(o.(*corev1alpha1.Trigger).Spec.CredentialRef)
		}},
		{&corev1alpha1.Credential{}, idxCredentialSecret, func(o client.Object) []string {
			if name := o.(*corev1alpha1.Credential).Spec.SecretRef.Name; name != "" {
				return []string{name}
			}
			return nil
		}},
	}
}

// enqueueByIndex builds an event handler that, when a watched dependency
// changes, enqueues only the resources whose index entry names it — replacing
// a namespace-wide fan-out.
//
// newList must return a fresh, empty list of the owning kind. fields are the
// index keys to consult; a resource matching any of them is enqueued once.
// Listing several fields matters where one kind reaches another by more than one
// route (an Agent references Tools directly *and* via ToolSets).
func enqueueByIndex(c client.Client, newList func() client.ObjectList, fields ...string) handler.EventHandler {
	return handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, obj client.Object) []reconcile.Request {
		return requestsMatchingIndex(ctx, c, newList, obj.GetNamespace(), obj.GetName(), fields...)
	})
}

// requestsMatchingIndex lists the owning kind once per field and dedupes.
func requestsMatchingIndex(
	ctx context.Context,
	c client.Client,
	newList func() client.ObjectList,
	namespace, name string,
	fields ...string,
) []reconcile.Request {
	seen := map[types.NamespacedName]bool{}
	var reqs []reconcile.Request
	for _, field := range fields {
		list := newList()
		if err := c.List(ctx, list,
			client.InNamespace(namespace),
			client.MatchingFields{field: name},
		); err != nil {
			// A failed List must not silently drop the event: fall back to nothing
			// here and let the caller's periodic resync converge. Returning partial
			// results is still correct — every field is independent.
			continue
		}
		items, err := meta.ExtractList(list)
		if err != nil {
			continue
		}
		for _, item := range items {
			acc, err := meta.Accessor(item)
			if err != nil {
				continue
			}
			key := types.NamespacedName{Namespace: acc.GetNamespace(), Name: acc.GetName()}
			if seen[key] {
				continue
			}
			seen[key] = true
			reqs = append(reqs, reconcile.Request{NamespacedName: key})
		}
	}
	return reqs
}

// agentsReferencingIndexed maps a changed dependency to the Agents that use it,
// following the two routes by which an Agent reaches a dependency *without*
// naming it directly:
//
//   - Inheritance. ApplyClassDefaults fills an unset modelRef/workflowRef/
//     promptRef/policyRefs from the Agent's AgentClass, so an Agent that names no
//     Model still breaks when the class's default Model changes. The index on the
//     Agent cannot see that; we resolve it by finding the AgentClasses that name
//     the dependency and then the Agents of those classes.
//   - Expansion. A ToolSet contributes its member Tools to every Agent that
//     references the set, so a Tool change must reach those Agents too.
//
// Missing either route would make the watch *lose* events, which is strictly
// worse than the namespace-coarse fan-out this replaces.
func (r *AgentReconciler) agentsReferencingIndexed(directFields []string, classFields []string, viaToolSet bool) handler.EventHandler {
	newAgents := func() client.ObjectList { return &corev1alpha1.AgentList{} }

	return handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, obj client.Object) []reconcile.Request {
		ns, name := obj.GetNamespace(), obj.GetName()
		seen := map[types.NamespacedName]bool{}
		var reqs []reconcile.Request
		add := func(rs []reconcile.Request) {
			for _, req := range rs {
				if !seen[req.NamespacedName] {
					seen[req.NamespacedName] = true
					reqs = append(reqs, req)
				}
			}
		}

		// Agents naming the dependency directly.
		if len(directFields) > 0 {
			add(requestsMatchingIndex(ctx, r.Client, newAgents, ns, name, directFields...))
		}

		// Agents inheriting it from an AgentClass that names it.
		for _, classField := range classFields {
			var classes corev1alpha1.AgentClassList
			if err := r.List(ctx, &classes,
				client.InNamespace(ns),
				client.MatchingFields{classField: name},
			); err != nil {
				continue
			}
			for i := range classes.Items {
				add(requestsMatchingIndex(ctx, r.Client, newAgents, ns, classes.Items[i].Name, idxAgentClass))
			}
		}

		// Agents reaching a Tool through a ToolSet that includes it.
		if viaToolSet {
			var sets corev1alpha1.ToolSetList
			if err := r.List(ctx, &sets,
				client.InNamespace(ns),
				client.MatchingFields{idxToolSetTools: name},
			); err == nil {
				for i := range sets.Items {
					add(requestsMatchingIndex(ctx, r.Client, newAgents, ns, sets.Items[i].Name, idxAgentToolSets))
				}
			}
		}

		return reqs
	})
}

// enqueueSkillsForConfigMap is the ConfigMap -> Skill mapping, kept separate
// because ConfigMap is a core type rather than an Agent Plane kind.
func enqueueSkillsForConfigMap(c client.Client) handler.EventHandler {
	return handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, obj client.Object) []reconcile.Request {
		if _, ok := obj.(*corev1.ConfigMap); !ok {
			return nil
		}
		return requestsMatchingIndex(ctx, c,
			func() client.ObjectList { return &corev1alpha1.SkillList{} },
			obj.GetNamespace(), obj.GetName(), idxSkillConfigMap)
	})
}
