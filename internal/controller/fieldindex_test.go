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
	"sort"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/util/workqueue"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	corev1alpha1 "github.com/hkmdxlftjf/agent-plane/api/v1alpha1"
)

// indexedClient builds a fake client wired with the *production* index
// extractors, so these tests fail if an extractor starts reading the wrong
// field.
func indexedClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("add client-go scheme: %v", err)
	}
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add agent-plane scheme: %v", err)
	}
	b := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...)
	for _, s := range indexSpecs() {
		b = b.WithIndex(s.obj, s.field, client.IndexerFunc(s.values))
	}
	return b.Build()
}

// collect drives an EventHandler the way controller-runtime does and returns the
// enqueued Agent names, sorted.
func collect(t *testing.T, h handler.EventHandler, changed client.Object) []string {
	t.Helper()
	q := &recordingQueue{}
	h.Create(context.Background(), event.CreateEvent{Object: changed}, q)
	names := make([]string, 0, len(q.items))
	for _, req := range q.items {
		names = append(names, req.Name)
	}
	sort.Strings(names)
	return names
}

// recordingQueue captures what a handler enqueues. Only Add is exercised;
// the rest satisfy the interface.
type recordingQueue struct {
	workqueue.TypedRateLimitingInterface[reconcile.Request]
	items []reconcile.Request
}

func (q *recordingQueue) Add(item reconcile.Request) { q.items = append(q.items, item) }
func (q *recordingQueue) AddRateLimited(item reconcile.Request) {
	q.items = append(q.items, item)
}
func (q *recordingQueue) AddAfter(item reconcile.Request, _ time.Duration) {
	q.items = append(q.items, item)
}

func agentNamed(name string, mutate func(*corev1alpha1.Agent)) *corev1alpha1.Agent {
	a := &corev1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: nsDefault}}
	mutate(a)
	return a
}

func ref(name string) *corev1alpha1.LocalReference { return &corev1alpha1.LocalReference{Name: name} }
func refs(names ...string) []corev1alpha1.LocalReference {
	out := make([]corev1alpha1.LocalReference, 0, len(names))
	for _, n := range names {
		out = append(out, corev1alpha1.LocalReference{Name: n})
	}
	return out
}

func modelNamed(name string) *corev1alpha1.Model {
	return &corev1alpha1.Model{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: nsDefault}}
}

// The point of the index: an Agent that names the changed Model is enqueued,
// and an unrelated Agent is not. Before this, every Agent in the namespace was.
func TestIndexedWatchIsPrecise(t *testing.T) {
	c := indexedClient(t,
		agentNamed("uses-it", func(a *corev1alpha1.Agent) { a.Spec.ModelRef = ref("m1") }),
		agentNamed("uses-other", func(a *corev1alpha1.Agent) { a.Spec.ModelRef = ref("m2") }),
		agentNamed("uses-none", func(a *corev1alpha1.Agent) {}),
	)
	r := &AgentReconciler{Client: c}

	got := collect(t, r.agentsReferencingIndexed([]string{idxAgentModel}, []string{idxClassModel}, false), modelNamed("m1"))
	if len(got) != 1 || got[0] != "uses-it" {
		t.Errorf("enqueued %v, want only [uses-it]", got)
	}
}

// An Agent with no modelRef of its own still breaks when its AgentClass's
// default Model changes. The index on the Agent cannot see that, so the handler
// resolves it through the class — missing this would LOSE an event, which is
// worse than the coarse fan-out being replaced.
func TestIndexedWatchFollowsAgentClassInheritance(t *testing.T) {
	c := indexedClient(t,
		&corev1alpha1.AgentClass{
			ObjectMeta: metav1.ObjectMeta{Name: "std", Namespace: nsDefault},
			Spec:       corev1alpha1.AgentClassSpec{DefaultModelRef: ref("shared-model")},
		},
		// Names no model; inherits it from the class.
		agentNamed("inherits", func(a *corev1alpha1.Agent) { a.Spec.AgentClassRef = ref("std") }),
		// In the same class but overrides the model, so the shared one is irrelevant.
		agentNamed("overrides", func(a *corev1alpha1.Agent) {
			a.Spec.AgentClassRef = ref("std")
			a.Spec.ModelRef = ref("own-model")
		}),
		agentNamed("unrelated", func(a *corev1alpha1.Agent) { a.Spec.ModelRef = ref("other") }),
	)
	r := &AgentReconciler{Client: c}
	h := r.agentsReferencingIndexed([]string{idxAgentModel}, []string{idxClassModel}, false)

	got := collect(t, h, modelNamed("shared-model"))
	// "overrides" is enqueued too: it is in the class, and deciding it is
	// unaffected would mean replicating ApplyClassDefaults in the mapper. Being
	// slightly over-inclusive here is safe; being under-inclusive is not.
	if len(got) < 1 || got[0] != "inherits" {
		t.Errorf("enqueued %v, want at least [inherits]", got)
	}
	for _, name := range got {
		if name == "unrelated" {
			t.Error("an Agent outside the class was enqueued")
		}
	}
}

// A Tool reaches an Agent directly or through a ToolSet. Both routes must fire.
func TestIndexedWatchFollowsToolSetExpansion(t *testing.T) {
	c := indexedClient(t,
		&corev1alpha1.ToolSet{
			ObjectMeta: metav1.ObjectMeta{Name: testToolSetName, Namespace: nsDefault},
			Spec:       corev1alpha1.ToolSetSpec{ToolRefs: refs("shared-tool")},
		},
		agentNamed("direct", func(a *corev1alpha1.Agent) { a.Spec.ToolRefs = refs("shared-tool") }),
		agentNamed("via-set", func(a *corev1alpha1.Agent) { a.Spec.ToolSetRefs = refs(testToolSetName) }),
		agentNamed("unrelated", func(a *corev1alpha1.Agent) { a.Spec.ToolRefs = refs("other-tool") }),
	)
	r := &AgentReconciler{Client: c}
	h := r.agentsReferencingIndexed([]string{idxAgentTools}, nil, true)

	got := collect(t, h, &corev1alpha1.Tool{
		ObjectMeta: metav1.ObjectMeta{Name: "shared-tool", Namespace: nsDefault},
	})
	if len(got) != 2 || got[0] != "direct" || got[1] != "via-set" {
		t.Errorf("enqueued %v, want [direct via-set]", got)
	}
}

// An Agent reachable by two routes at once must be enqueued once, not twice.
func TestIndexedWatchDedupes(t *testing.T) {
	c := indexedClient(t,
		&corev1alpha1.ToolSet{
			ObjectMeta: metav1.ObjectMeta{Name: testToolSetName, Namespace: nsDefault},
			Spec:       corev1alpha1.ToolSetSpec{ToolRefs: refs("shared-tool")},
		},
		agentNamed("both-routes", func(a *corev1alpha1.Agent) {
			a.Spec.ToolRefs = refs("shared-tool")
			a.Spec.ToolSetRefs = refs(testToolSetName)
		}),
	)
	r := &AgentReconciler{Client: c}
	h := r.agentsReferencingIndexed([]string{idxAgentTools}, nil, true)

	got := collect(t, h, &corev1alpha1.Tool{
		ObjectMeta: metav1.ObjectMeta{Name: "shared-tool", Namespace: nsDefault},
	})
	if len(got) != 1 {
		t.Errorf("enqueued %v, want one entry", got)
	}
}

// A dependency nothing references must enqueue nothing — the whole point.
func TestIndexedWatchIgnoresUnreferenced(t *testing.T) {
	c := indexedClient(t,
		agentNamed("a", func(a *corev1alpha1.Agent) { a.Spec.ModelRef = ref("m1") }),
	)
	r := &AgentReconciler{Client: c}

	got := collect(t, r.agentsReferencingIndexed([]string{idxAgentModel}, []string{idxClassModel}, false),
		modelNamed("nobody-uses-me"))
	if len(got) != 0 {
		t.Errorf("enqueued %v, want none", got)
	}
}

// Namespace isolation: an index match in another namespace must not leak.
func TestIndexedWatchIsNamespaceScoped(t *testing.T) {
	other := &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "elsewhere", Namespace: "other-ns"},
		Spec:       corev1alpha1.AgentSpec{ModelRef: ref("m1")},
	}
	c := indexedClient(t,
		agentNamed("here", func(a *corev1alpha1.Agent) { a.Spec.ModelRef = ref("m1") }),
		other,
	)
	r := &AgentReconciler{Client: c}

	got := collect(t, r.agentsReferencingIndexed([]string{idxAgentModel}, []string{idxClassModel}, false),
		modelNamed("m1"))
	if len(got) != 1 || got[0] != "here" {
		t.Errorf("enqueued %v, want only the same-namespace Agent", got)
	}
}

// The non-Agent controllers use the generic helper; check one of each shape.
func TestEnqueueByIndex(t *testing.T) {
	c := indexedClient(t,
		&corev1alpha1.Model{
			ObjectMeta: metav1.ObjectMeta{Name: "uses-cred", Namespace: nsDefault},
			Spec:       corev1alpha1.ModelSpec{CredentialRef: ref("shared-cred")},
		},
		&corev1alpha1.Model{
			ObjectMeta: metav1.ObjectMeta{Name: "no-cred", Namespace: nsDefault},
		},
	)
	h := enqueueByIndex(c, func() client.ObjectList { return &corev1alpha1.ModelList{} }, idxModelCredential)

	got := collect(t, h, &corev1alpha1.Credential{
		ObjectMeta: metav1.ObjectMeta{Name: "shared-cred", Namespace: nsDefault},
	})
	if len(got) != 1 || got[0] != "uses-cred" {
		t.Errorf("enqueued %v, want only [uses-cred]", got)
	}
}

// ConfigMap -> Skill goes through its own handler because ConfigMap is a core
// type.
func TestEnqueueSkillsForConfigMap(t *testing.T) {
	c := indexedClient(t,
		&corev1alpha1.Skill{
			ObjectMeta: metav1.ObjectMeta{Name: "from-cm", Namespace: nsDefault},
			Spec: corev1alpha1.SkillSpec{
				Description:         "sourced from a ConfigMap",
				ContentConfigMapRef: &corev1alpha1.ConfigMapKeyReference{Name: "skill-cm", Key: testSkillCMKey},
			},
		},
		&corev1alpha1.Skill{
			ObjectMeta: metav1.ObjectMeta{Name: contentSourceInline, Namespace: nsDefault},
			Spec:       corev1alpha1.SkillSpec{Description: contentSourceInline, Content: "# skill body"},
		},
	)
	h := enqueueSkillsForConfigMap(c)

	got := collect(t, h, &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "skill-cm", Namespace: nsDefault},
	})
	if len(got) != 1 || got[0] != "from-cm" {
		t.Errorf("enqueued %v, want only [from-cm]", got)
	}
}

// An unset optional reference must not be indexed under the empty string —
// otherwise every Agent without a modelRef would collide on one key and a
// stray lookup could enqueue all of them.
func TestUnsetReferencesAreNotIndexed(t *testing.T) {
	if got := refName(nil); got != nil {
		t.Errorf("refName(nil) = %v, want nil", got)
	}
	if got := refName(&corev1alpha1.LocalReference{Name: ""}); got != nil {
		t.Errorf("refName(empty) = %v, want nil", got)
	}
	if got := refNamesOf(nil); got != nil {
		t.Errorf("refNamesOf(nil) = %v, want nil", got)
	}
	if got := refNamesOf(refs("a", "b")); len(got) != 2 {
		t.Errorf("refNamesOf = %v, want two entries", got)
	}
}

// Registering the same (type, field) twice is rejected by controller-runtime, so
// the spec list must not contain a duplicate pair.
func TestIndexSpecsHaveNoDuplicates(t *testing.T) {
	type key struct {
		typ   string
		field string
	}
	seen := map[key]bool{}
	for _, s := range indexSpecs() {
		k := key{typ: fmt.Sprintf("%T", s.obj), field: s.field}
		if seen[k] {
			t.Errorf("duplicate index registration: %s by %s", k.typ, k.field)
		}
		seen[k] = true
	}
}

// The convergence property the watches exist for: an Agent Degraded because a
// dependency does not exist yet must still be enqueued when that dependency is
// finally created. The index is built from the Agent's spec, which names the
// missing object, so this works — but it is the property most easily broken by
// a precision change, and README/usage.md both promise it.
func TestIndexedWatchEnqueuesAgentsWaitingOnAMissingDependency(t *testing.T) {
	// The Model does not exist; the Agent references it by name regardless.
	c := indexedClient(t,
		agentNamed("waiting", func(a *corev1alpha1.Agent) { a.Spec.ModelRef = ref("not-yet-created") }),
	)
	r := &AgentReconciler{Client: c}
	h := r.agentsReferencingIndexed([]string{idxAgentModel}, []string{idxClassModel}, false)

	// Simulate the Model finally being created.
	got := collect(t, h, modelNamed("not-yet-created"))
	if len(got) != 1 || got[0] != "waiting" {
		t.Errorf("enqueued %v, want [waiting] — a Degraded Agent must converge", got)
	}
}

// Same for the indirect route: an Agent waiting on a Tool it reaches through a
// ToolSet must converge when the Tool appears.
func TestIndexedWatchConvergesThroughToolSet(t *testing.T) {
	c := indexedClient(t,
		&corev1alpha1.ToolSet{
			ObjectMeta: metav1.ObjectMeta{Name: testToolSetName, Namespace: nsDefault},
			Spec:       corev1alpha1.ToolSetSpec{ToolRefs: refs("not-yet-created")},
		},
		agentNamed("waiting", func(a *corev1alpha1.Agent) { a.Spec.ToolSetRefs = refs(testToolSetName) }),
	)
	r := &AgentReconciler{Client: c}
	h := r.agentsReferencingIndexed([]string{idxAgentTools}, nil, true)

	got := collect(t, h, &corev1alpha1.Tool{
		ObjectMeta: metav1.ObjectMeta{Name: "not-yet-created", Namespace: nsDefault},
	})
	if len(got) != 1 || got[0] != "waiting" {
		t.Errorf("enqueued %v, want [waiting]", got)
	}
}

// Every kind the Agent controller watches must have a working index; a missing
// or misspelled one silently stops that dependency from ever re-reconciling its
// Agents. This walks all of them rather than trusting the wiring by eye.
func TestEveryAgentDependencyKindIsIndexed(t *testing.T) {
	const dep = "the-dep"
	cases := []struct {
		name  string
		field string
		spec  func(*corev1alpha1.Agent)
	}{
		{"model", idxAgentModel, func(a *corev1alpha1.Agent) { a.Spec.ModelRef = ref(dep) }},
		{"workflow", idxAgentWorkflow, func(a *corev1alpha1.Agent) { a.Spec.WorkflowRef = ref(dep) }},
		{"prompt", idxAgentPrompt, func(a *corev1alpha1.Agent) { a.Spec.PromptRef = ref(dep) }},
		{"tools", idxAgentTools, func(a *corev1alpha1.Agent) { a.Spec.ToolRefs = refs(dep) }},
		{"toolSets", idxAgentToolSets, func(a *corev1alpha1.Agent) { a.Spec.ToolSetRefs = refs(dep) }},
		{"skills", idxAgentSkills, func(a *corev1alpha1.Agent) { a.Spec.SkillRefs = refs(dep) }},
		{"memories", idxAgentMemories, func(a *corev1alpha1.Agent) { a.Spec.MemoryRefs = refs(dep) }},
		{"policies", idxAgentPolicies, func(a *corev1alpha1.Agent) { a.Spec.PolicyRefs = refs(dep) }},
		{"toolPolicies", idxAgentToolPolicies, func(a *corev1alpha1.Agent) { a.Spec.ToolPolicyRefs = refs(dep) }},
		{"knowledgeBases", idxAgentKnowledge, func(a *corev1alpha1.Agent) { a.Spec.KnowledgeBaseRefs = refs(dep) }},
		{"agentClass", idxAgentClass, func(a *corev1alpha1.Agent) { a.Spec.AgentClassRef = ref(dep) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := indexedClient(t, agentNamed("referrer", tc.spec))
			r := &AgentReconciler{Client: c}
			h := r.agentsReferencingIndexed([]string{tc.field}, nil, false)

			got := collect(t, h, modelNamed(dep)) // the changed object's kind is irrelevant to the mapper
			if len(got) != 1 || got[0] != "referrer" {
				t.Errorf("enqueued %v, want [referrer] — %s is not indexed correctly", got, tc.field)
			}
		})
	}
}

// The Trigger indexes back its two watches: an Agent gaining a runtime port, or
// a Credential changing, must re-reconcile the Triggers that point at them.
func TestTriggerIndexes(t *testing.T) {
	c := indexedClient(t,
		&corev1alpha1.Trigger{
			ObjectMeta: metav1.ObjectMeta{Name: testTriggerName, Namespace: nsDefault},
			Spec: corev1alpha1.TriggerSpec{
				AgentRef:      corev1alpha1.LocalReference{Name: "support"},
				Image:         testAdapterImage,
				CredentialRef: ref("lark-app"),
			},
		},
		&corev1alpha1.Trigger{
			ObjectMeta: metav1.ObjectMeta{Name: "other", Namespace: nsDefault},
			Spec: corev1alpha1.TriggerSpec{
				AgentRef: corev1alpha1.LocalReference{Name: "different-agent"},
				Image:    testAdapterImage,
			},
		},
	)
	triggers := func() client.ObjectList { return &corev1alpha1.TriggerList{} }

	byAgent := collect(t, enqueueByIndex(c, triggers, idxTriggerAgent), &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "support", Namespace: nsDefault},
	})
	if len(byAgent) != 1 || byAgent[0] != testTriggerName {
		t.Errorf("by agent: enqueued %v, want the referring trigger", byAgent)
	}

	byCred := collect(t, enqueueByIndex(c, triggers, idxTriggerCredential), &corev1alpha1.Credential{
		ObjectMeta: metav1.ObjectMeta{Name: "lark-app", Namespace: nsDefault},
	})
	if len(byCred) != 1 || byCred[0] != testTriggerName {
		t.Errorf("by credential: enqueued %v, want the referring trigger", byCred)
	}

	// A Trigger with no credentialRef must not be indexed under an empty key.
	none := collect(t, enqueueByIndex(c, triggers, idxTriggerCredential), &corev1alpha1.Credential{
		ObjectMeta: metav1.ObjectMeta{Name: "", Namespace: nsDefault},
	})
	if len(none) != 0 {
		t.Errorf("empty credential name matched %v", none)
	}
}
