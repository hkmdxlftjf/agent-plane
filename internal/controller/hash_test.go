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

import "testing"

// TestConfigHashDeterminism verifies the property the Registry and runtimes
// rely on: identical resolved references produce an identical hash, and any
// change (a ref added, or a referenced object's ResourceVersion bumped)
// produces a different hash.
func TestConfigHashDeterminism(t *testing.T) {
	base := []resolvedRef{
		{Kind: kindModel, Name: "m", ResourceVersion: "1"},
		{Kind: kindTool, Name: "s", ResourceVersion: "7"},
	}

	h1, err := configHash(base)
	if err != nil {
		t.Fatalf("configHash returned error: %v", err)
	}

	// Same input -> same hash.
	same := []resolvedRef{
		{Kind: kindModel, Name: "m", ResourceVersion: "1"},
		{Kind: kindTool, Name: "s", ResourceVersion: "7"},
	}
	h2, err := configHash(same)
	if err != nil {
		t.Fatalf("configHash returned error: %v", err)
	}
	if h1 != h2 {
		t.Errorf("expected identical hashes for identical input, got %q and %q", h1, h2)
	}

	// A bumped ResourceVersion changes the hash (dependency changed).
	bumped := []resolvedRef{
		{Kind: kindModel, Name: "m", ResourceVersion: "2"},
		{Kind: kindTool, Name: "s", ResourceVersion: "7"},
	}
	h3, err := configHash(bumped)
	if err != nil {
		t.Fatalf("configHash returned error: %v", err)
	}
	if h1 == h3 {
		t.Errorf("expected different hash when a ResourceVersion changes, both were %q", h1)
	}

	// An added reference changes the hash.
	added := append([]resolvedRef{}, base...)
	added = append(added, resolvedRef{Kind: kindMemory, Name: "mem", ResourceVersion: "1"})
	h4, err := configHash(added)
	if err != nil {
		t.Fatalf("configHash returned error: %v", err)
	}
	if h1 == h4 {
		t.Errorf("expected different hash when a reference is added, both were %q", h1)
	}
}
