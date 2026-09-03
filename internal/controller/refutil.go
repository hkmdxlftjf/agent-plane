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
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
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
// reference-resolving controllers (Model, Tool, ToolSet, AgentClass); the Agent
// controller keeps its own variant because it additionally needs
// ResourceVersions for config hashing.
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
