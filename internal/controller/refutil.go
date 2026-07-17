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

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// refTarget describes one reference to resolve during reconciliation: a kind
// label (for human-readable messages), the referenced object's name, and an
// empty object of the right type to Get into.
type refTarget struct {
	kind string
	name string
	obj  client.Object
}

// resolveTargets attempts to Get every target in the given namespace and
// returns the "Kind/name" of any that do not exist. A get error other than
// NotFound aborts and is returned. This is the shared building block behind the
// reference-resolving controllers (Model, Memory, Tool, ToolSet,
// KnowledgeBase, AgentClass); the Agent controller keeps its own variant
// because it additionally needs ResourceVersions for config hashing.
func resolveTargets(ctx context.Context, c client.Client, ns string, targets []refTarget) ([]string, error) {
	var missing []string
	for _, t := range targets {
		err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: t.name}, t.obj)
		switch {
		case err == nil:
			// resolved
		case apierrors.IsNotFound(err):
			missing = append(missing, t.kind+"/"+t.name)
		default:
			return nil, err
		}
	}
	return missing, nil
}

// enqueueReferrers builds an event handler that, when a watched dependency
// changes, enqueues every resource of the owning kind in the same namespace.
// newList must return a fresh, empty list of the owning kind (e.g.
// func() client.ObjectList { return &corev1alpha1.ToolSetList{} }).
//
// The enqueue is intentionally namespace-coarse rather than
// reference-precise: reconciling a resource whose references did not actually
// change is cheap (a few gets + an idempotent status update), and it avoids
// maintaining a reverse index. This mirrors the Agent controller's approach and
// is what makes the reference graph converge — e.g. a ToolSet stuck Degraded
// becomes Ready as soon as the missing Tool is created. A field-indexed,
// reference-precise variant is a documented follow-up.
func enqueueReferrers(c client.Client, newList func() client.ObjectList) handler.EventHandler {
	return handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, obj client.Object) []reconcile.Request {
		list := newList()
		if err := c.List(ctx, list, client.InNamespace(obj.GetNamespace())); err != nil {
			return nil
		}
		items, err := meta.ExtractList(list)
		if err != nil {
			return nil
		}
		reqs := make([]reconcile.Request, 0, len(items))
		for _, item := range items {
			acc, err := meta.Accessor(item)
			if err != nil {
				continue
			}
			reqs = append(reqs, reconcile.Request{NamespacedName: types.NamespacedName{
				Namespace: acc.GetNamespace(),
				Name:      acc.GetName(),
			}})
		}
		return reqs
	})
}
