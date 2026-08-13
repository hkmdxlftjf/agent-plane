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
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	corev1alpha1 "github.com/hkmdxlftjf/agent-plane/api/v1alpha1"
)

// repoAgent builds an Agent bound to a repository, which is the shape a coding
// agent (Claude Code, Codex, OpenCode) runs in: one pod, one working tree.
func repoAgent(name, repo string, mutate func(*corev1alpha1.Agent)) *corev1alpha1.Agent {
	a := &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: nsDefault},
		Spec: corev1alpha1.AgentSpec{
			ModelRef: &corev1alpha1.LocalReference{Name: testAnyModel},
			Runtime: &corev1alpha1.AgentRuntimeSpec{
				Image: testCodingImage,
				Port:  8080,
			},
			Workspace: &corev1alpha1.AgentWorkspaceSpec{Repository: repo},
		},
	}
	if mutate != nil {
		mutate(a)
	}
	return a
}

var _ = Describe("Agent workspace", func() {
	ctx := context.Background()

	Context("When an Agent is bound to a repository", func() {
		const (
			agentName = "test-ws-agent"
			modelName = "test-ws-model"
		)
		agentKey := types.NamespacedName{Name: agentName, Namespace: nsDefault}
		depKey := types.NamespacedName{Name: agentName + "-runtime", Namespace: nsDefault}
		pvcKey := types.NamespacedName{Name: agentName + "-workspace", Namespace: nsDefault}

		BeforeEach(func() {
			model := &corev1alpha1.Model{
				ObjectMeta: metav1.ObjectMeta{Name: modelName, Namespace: nsDefault},
				Spec:       corev1alpha1.ModelSpec{Provider: testProvider, ModelName: testModelName},
			}
			Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, model))).To(Succeed())

			agent := repoAgent(agentName, "https://github.com/org/api", func(a *corev1alpha1.Agent) {
				a.Spec.ModelRef = &corev1alpha1.LocalReference{Name: modelName}
				a.Spec.Workspace.Branch = "main"
				a.Spec.Workspace.Size = "20Gi"
			})
			Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, agent))).To(Succeed())
		})

		AfterEach(func() {
			deleteIfExists(ctx, agentKey, &corev1alpha1.Agent{})
			deleteIfExists(ctx, depKey, &appsv1.Deployment{})
			deleteIfExists(ctx, pvcKey, &corev1.PersistentVolumeClaim{})
			deleteIfExists(ctx, types.NamespacedName{Name: modelName, Namespace: nsDefault}, &corev1alpha1.Model{})
		})

		It("provisions a working tree and mounts it into the runtime", func() {
			reconciler := &AgentReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: agentKey})
			Expect(err).NotTo(HaveOccurred())

			By("provisioning the PersistentVolumeClaim")
			pvc := &corev1.PersistentVolumeClaim{}
			Expect(k8sClient.Get(ctx, pvcKey, pvc)).To(Succeed())
			Expect(pvc.Spec.AccessModes).To(ConsistOf(corev1.ReadWriteOnce))
			Expect(pvc.Spec.Resources.Requests.Storage().String()).To(Equal("20Gi"))
			Expect(pvc.OwnerReferences).To(HaveLen(1))

			By("cloning into it before the runtime starts")
			dep := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, depKey, dep)).To(Succeed())
			Expect(dep.Spec.Template.Spec.InitContainers).To(HaveLen(1))
			clone := dep.Spec.Template.Spec.InitContainers[0]
			Expect(clone.Name).To(Equal("clone"))
			Expect(envOf(clone)["AGENTPLANE_REPOSITORY"]).To(Equal("https://github.com/org/api"))
			Expect(envOf(clone)["AGENTPLANE_BRANCH"]).To(Equal("main"))
			Expect(clone.VolumeMounts).To(HaveLen(1))
			Expect(clone.VolumeMounts[0].MountPath).To(Equal("/workspace"))

			By("mounting the same tree into the runtime container")
			runtime := dep.Spec.Template.Spec.Containers[0]
			Expect(runtime.VolumeMounts).To(HaveLen(2))
			Expect(runtime.VolumeMounts[0].Name).To(Equal(workspaceVolumeName))
			Expect(envOf(runtime)["AGENTPLANE_WORKSPACE"]).To(Equal("/workspace"))
			Expect(dep.Spec.Template.Spec.Volumes[0].PersistentVolumeClaim.ClaimName).To(Equal(pvcKey.Name))

			By("declaring the cloned tree safe for a different uid")
			// The clone container runs as root; the runtime image drops to a non-root
			// user. Without this, git refuses the tree as "dubious ownership" and
			// every command the agent exists to run fails on a checkout that is
			// otherwise perfectly good.
			Expect(envOf(runtime)["GIT_CONFIG_COUNT"]).To(Equal("1"))
			Expect(envOf(runtime)["GIT_CONFIG_KEY_0"]).To(Equal("safe.directory"))
			Expect(envOf(runtime)["GIT_CONFIG_VALUE_0"]).To(Equal("/workspace"))

			By("pinning to a single writer")
			// A working tree has exactly one writer. RollingUpdate would overlap two
			// pods on the same checkout, and with ReadWriteOnce the new one could not
			// even schedule — so Recreate at one replica is the only correct pairing.
			Expect(*dep.Spec.Replicas).To(Equal(int32(1)))
			Expect(dep.Spec.Strategy.Type).To(Equal(appsv1.RecreateDeploymentStrategyType))

			By("sandboxing the container that runs model-directed shell commands")
			sc := runtime.SecurityContext
			Expect(sc).NotTo(BeNil())
			Expect(*sc.RunAsNonRoot).To(BeTrue())
			Expect(*sc.ReadOnlyRootFilesystem).To(BeTrue())
			Expect(*sc.AllowPrivilegeEscalation).To(BeFalse())
			Expect(sc.Capabilities.Drop).To(ConsistOf(corev1.Capability("ALL")))

			agent := &corev1alpha1.Agent{}
			Expect(k8sClient.Get(ctx, agentKey, agent)).To(Succeed())
			Expect(agent.Status.WorkspaceClaim).To(Equal(pvcKey.Name))
		})

		It("does not rewrite the claim on later reconciles", func() {
			reconciler := &AgentReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: agentKey})
			Expect(err).NotTo(HaveOccurred())

			first := &corev1.PersistentVolumeClaim{}
			Expect(k8sClient.Get(ctx, pvcKey, first)).To(Succeed())

			// A PVC's storage request is immutable in most clusters, so re-applying
			// the spec would fail against an unchanged claim.
			for range 3 {
				_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: agentKey})
				Expect(err).NotTo(HaveOccurred())
			}

			after := &corev1.PersistentVolumeClaim{}
			Expect(k8sClient.Get(ctx, pvcKey, after)).To(Succeed())
			Expect(after.ResourceVersion).To(Equal(first.ResourceVersion))
		})
	})

	// The git credential must reach the clone step as a mounted file, never as an
	// environment value or inside the remote URL — a URL-embedded token leaks into
	// `git remote -v`, the reflog, and any error git prints.
	Context("When the workspace has a credential", func() {
		const (
			agentName  = "test-ws-cred-agent"
			credName   = "test-ws-cred"
			secretName = "test-ws-secret"
		)
		agentKey := types.NamespacedName{Name: agentName, Namespace: nsDefault}
		depKey := types.NamespacedName{Name: agentName + "-runtime", Namespace: nsDefault}
		pvcKey := types.NamespacedName{Name: agentName + "-workspace", Namespace: nsDefault}

		BeforeEach(func() {
			cred := &corev1alpha1.Credential{
				ObjectMeta: metav1.ObjectMeta{Name: credName, Namespace: nsDefault},
				Spec: corev1alpha1.CredentialSpec{
					SecretRef: corev1alpha1.SecretKeyReference{Name: secretName, Key: "token"},
				},
			}
			Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, cred))).To(Succeed())

			agent := repoAgent(agentName, "https://github.com/org/private", func(a *corev1alpha1.Agent) {
				a.Spec.Workspace.CredentialRef = &corev1alpha1.LocalReference{Name: credName}
			})
			Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, agent))).To(Succeed())
		})

		AfterEach(func() {
			deleteIfExists(ctx, agentKey, &corev1alpha1.Agent{})
			deleteIfExists(ctx, depKey, &appsv1.Deployment{})
			deleteIfExists(ctx, pvcKey, &corev1.PersistentVolumeClaim{})
			deleteIfExists(ctx, types.NamespacedName{Name: credName, Namespace: nsDefault}, &corev1alpha1.Credential{})
		})

		It("mounts the credential and keeps it out of the URL and env", func() {
			reconciler := &AgentReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: agentKey})
			Expect(err).NotTo(HaveOccurred())

			dep := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, depKey, dep)).To(Succeed())
			clone := dep.Spec.Template.Spec.InitContainers[0]

			Expect(clone.VolumeMounts).To(HaveLen(2))
			var mounted bool
			for _, m := range clone.VolumeMounts {
				if m.MountPath == gitCredentialMountPath {
					mounted = true
					Expect(m.ReadOnly).To(BeTrue())
				}
			}
			Expect(mounted).To(BeTrue(), "credential is not mounted")

			// Only the *path* is injected; the value stays in the volume.
			Expect(envOf(clone)["AGENTPLANE_GIT_CREDENTIAL_FILE"]).To(HavePrefix(gitCredentialMountPath))
			Expect(envOf(clone)["AGENTPLANE_REPOSITORY"]).NotTo(ContainSubstring("@"))

			var secretVol bool
			for _, v := range dep.Spec.Template.Spec.Volumes {
				if v.Secret != nil && v.Secret.SecretName == secretName {
					secretVol = true
				}
			}
			Expect(secretVol).To(BeTrue())
		})

		// A coding agent that can only read its repository can never deliver a
		// change: the runtime container needs the same push credential, mounted
		// the same way, or it can edit files but never commit them back.
		It("also mounts the credential into the runtime container for push", func() {
			reconciler := &AgentReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: agentKey})
			Expect(err).NotTo(HaveOccurred())

			dep := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, depKey, dep)).To(Succeed())
			runtime := dep.Spec.Template.Spec.Containers[0]

			var mounted bool
			for _, m := range runtime.VolumeMounts {
				if m.MountPath == gitCredentialMountPath {
					mounted = true
					Expect(m.ReadOnly).To(BeTrue())
				}
			}
			Expect(mounted).To(BeTrue(), "push credential is not mounted into the runtime container")

			Expect(envOf(runtime)["AGENTPLANE_GIT_CREDENTIAL_FILE"]).To(HavePrefix(gitCredentialMountPath))
			Expect(envOf(runtime)["GIT_CONFIG_COUNT"]).To(Equal("2"))
			Expect(envOf(runtime)["GIT_CONFIG_KEY_1"]).To(Equal("credential.helper"))
			Expect(envOf(runtime)["GIT_CONFIG_VALUE_1"]).To(Equal(gitCredentialHelper))
		})
	})

	// A missing Credential must fail the reconcile rather than scheduling a pod
	// that crash-loops on an absent volume.
	Context("When the workspace credential is missing", func() {
		const agentName = "test-ws-badcred-agent"
		agentKey := types.NamespacedName{Name: agentName, Namespace: nsDefault}

		BeforeEach(func() {
			agent := repoAgent(agentName, "https://github.com/org/x", func(a *corev1alpha1.Agent) {
				a.Spec.Workspace.CredentialRef = &corev1alpha1.LocalReference{Name: testMissingName}
			})
			Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, agent))).To(Succeed())
		})

		AfterEach(func() {
			deleteIfExists(ctx, agentKey, &corev1alpha1.Agent{})
			deleteIfExists(ctx, types.NamespacedName{Name: agentName + "-runtime", Namespace: nsDefault}, &appsv1.Deployment{})
			deleteIfExists(ctx, types.NamespacedName{Name: agentName + "-workspace", Namespace: nsDefault}, &corev1.PersistentVolumeClaim{})
		})

		It("fails the reconcile", func() {
			reconciler := &AgentReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: agentKey})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring(testMissingName))
		})
	})

	// The runtime container's root filesystem is read-only, so a runtime that
	// keeps state (conversation history, resolved plugin dependencies) needs a
	// writable HOME — and one that survives a restart, or every restart starts
	// the conversation over. It goes on the workspace volume for that reason;
	// /tmp is writable but is an emptyDir.
	Context("When a workspace runtime needs a writable home", func() {
		const (
			agentName = "test-ws-home-agent"
			modelName = "test-ws-home-model"
			credName  = "test-ws-home-cred"
		)
		agentKey := types.NamespacedName{Name: agentName, Namespace: nsDefault}
		depKey := types.NamespacedName{Name: agentName + "-runtime", Namespace: nsDefault}

		BeforeEach(func() {
			cred := &corev1alpha1.Credential{
				ObjectMeta: metav1.ObjectMeta{Name: credName, Namespace: nsDefault},
				Spec: corev1alpha1.CredentialSpec{
					SecretRef: corev1alpha1.SecretKeyReference{Name: "model-secret", Key: "api-key"},
				},
			}
			Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, cred))).To(Succeed())

			model := &corev1alpha1.Model{
				ObjectMeta: metav1.ObjectMeta{Name: modelName, Namespace: nsDefault},
				Spec: corev1alpha1.ModelSpec{
					Provider:      testProvider,
					ModelName:     testModelName,
					CredentialRef: &corev1alpha1.LocalReference{Name: credName},
				},
			}
			Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, model))).To(Succeed())

			agent := repoAgent(agentName, "https://github.com/org/home", func(a *corev1alpha1.Agent) {
				a.Spec.ModelRef = &corev1alpha1.LocalReference{Name: modelName}
			})
			Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, agent))).To(Succeed())
		})

		AfterEach(func() {
			deleteIfExists(ctx, agentKey, &corev1alpha1.Agent{})
			deleteIfExists(ctx, depKey, &appsv1.Deployment{})
			deleteIfExists(ctx, types.NamespacedName{Name: agentName + "-workspace", Namespace: nsDefault}, &corev1.PersistentVolumeClaim{})
			deleteIfExists(ctx, types.NamespacedName{Name: modelName, Namespace: nsDefault}, &corev1alpha1.Model{})
			deleteIfExists(ctx, types.NamespacedName{Name: credName, Namespace: nsDefault}, &corev1alpha1.Credential{})
		})

		It("points HOME at the workspace volume and mounts the model credential", func() {
			reconciler := &AgentReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: agentKey})
			Expect(err).NotTo(HaveOccurred())

			dep := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, depKey, dep)).To(Succeed())
			runtime := dep.Spec.Template.Spec.Containers[0]
			env := envOf(runtime)

			By("placing HOME on the persistent volume, not in /tmp")
			home := "/workspace/" + agentHomeDirName
			Expect(env["HOME"]).To(Equal(home))
			Expect(env["AGENTPLANE_AGENT_HOME"]).To(Equal(home))
			// XDG_* are set explicitly because only some of them derive from HOME.
			Expect(env["XDG_CONFIG_HOME"]).To(HavePrefix(home))
			Expect(env["XDG_DATA_HOME"]).To(HavePrefix(home))
			Expect(env["XDG_STATE_HOME"]).To(HavePrefix(home))
			Expect(env["XDG_CACHE_HOME"]).To(HavePrefix(home))

			By("keeping that directory out of the repository")
			clone := dep.Spec.Template.Spec.InitContainers[0]
			Expect(clone.Command).To(ContainElement(ContainSubstring("info/exclude")))
			// The /** suffix matters: a repository whose own .gitignore re-includes
			// directories ("/*" then "!/*/") overrides a plain directory pattern
			// here, and the agent's state reappears as untracked files.
			Expect(clone.Command).To(ContainElement(ContainSubstring("/" + agentHomeDirName + "/**")))
			// Created as root, written by a non-root uid sharing only the fsGroup:
			// without the group-write bit the runtime cannot start on a fresh volume.
			Expect(clone.Command).To(ContainElement(ContainSubstring("chmod 2775")))

			By("reporting ready only once the runtime has projected its config")
			// Every startup failure this shape has hit looks the same from outside:
			// the port is bound, a health endpoint answers, and real requests hang.
			// A probe that only checks liveness would call that ready, so this one
			// asserts on the projected config instead.
			probe := runtime.ReadinessProbe
			Expect(probe).NotTo(BeNil(), "a workspace runtime with a port must have a readiness probe")
			Expect(probe.Exec).NotTo(BeNil(), "must inspect the response body, not just a status code")
			Expect(probe.Exec.Command).To(ContainElement(ContainSubstring("/config")))
			// Anchored on the selected model, not the bare provider id: the id also
			// occurs in the plugin's file path, so the loose form would pass on a boot
			// where projection never happened — the one case worth detecting.
			Expect(probe.Exec.Command).To(ContainElement(ContainSubstring(projectedModelMarker)))
			Expect(projectedModelMarker).NotTo(Equal("agent-plane"))
			// A cold first start legitimately takes minutes; slow must not read as broken.
			Expect(probe.FailureThreshold * probe.PeriodSeconds).To(BeNumerically(">=", 120))

			By("mounting the model credential read-only, so no Kubernetes client is needed")
			var mounted bool
			for _, m := range runtime.VolumeMounts {
				if m.MountPath == modelCredentialMountPath {
					mounted = true
					Expect(m.ReadOnly).To(BeTrue())
				}
			}
			Expect(mounted).To(BeTrue(), "model credential is not mounted into the runtime container")

			var fromSecret string
			for _, v := range dep.Spec.Template.Spec.Volumes {
				if v.Name == "model-credential" && v.Secret != nil {
					fromSecret = v.Secret.SecretName
				}
			}
			Expect(fromSecret).To(Equal("model-secret"))

			By("still refusing to write to the root filesystem")
			Expect(runtime.SecurityContext.ReadOnlyRootFilesystem).To(HaveValue(BeTrue()))
			Expect(runtime.SecurityContext.RunAsNonRoot).To(HaveValue(BeTrue()))
		})
	})

	// An Agent with no model credential must still get a Deployment: the
	// credential mount is additive, and a runtime that starts and logs the gap
	// converges once the Secret appears.
	Context("When a workspace Agent has no model credential", func() {
		const agentName = "test-ws-nocred-agent"
		agentKey := types.NamespacedName{Name: agentName, Namespace: nsDefault}
		depKey := types.NamespacedName{Name: agentName + "-runtime", Namespace: nsDefault}

		BeforeEach(func() {
			agent := repoAgent(agentName, "https://github.com/org/nocred", nil)
			Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, agent))).To(Succeed())
		})

		AfterEach(func() {
			deleteIfExists(ctx, agentKey, &corev1alpha1.Agent{})
			deleteIfExists(ctx, depKey, &appsv1.Deployment{})
			deleteIfExists(ctx, types.NamespacedName{Name: agentName + "-workspace", Namespace: nsDefault}, &corev1.PersistentVolumeClaim{})
		})

		It("still creates the Deployment, with a writable home and no credential volume", func() {
			reconciler := &AgentReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: agentKey})
			Expect(err).NotTo(HaveOccurred())

			dep := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, depKey, dep)).To(Succeed())
			runtime := dep.Spec.Template.Spec.Containers[0]
			Expect(envOf(runtime)["HOME"]).To(Equal("/workspace/" + agentHomeDirName))
			for _, m := range runtime.VolumeMounts {
				Expect(m.MountPath).NotTo(Equal(modelCredentialMountPath))
			}
		})
	})

	// A runtime that reads its own model credential needs an identity to read it
	// with. Without this field the pods run as the namespace's "default"
	// ServiceAccount, and the only way to grant the Secret read is to bind it to
	// "default" — which grants it to every unrelated pod in the namespace too.
	Context("When an Agent overrides readiness and picks a sandboxed runtime", func() {
		const agentName = "test-sandboxed-agent"
		agentKey := types.NamespacedName{Name: agentName, Namespace: nsDefault}
		depKey := types.NamespacedName{Name: agentName + "-runtime", Namespace: nsDefault}

		BeforeEach(func() {
			agent := &corev1alpha1.Agent{
				ObjectMeta: metav1.ObjectMeta{Name: agentName, Namespace: nsDefault},
				Spec: corev1alpha1.AgentSpec{
					ModelRef: &corev1alpha1.LocalReference{Name: testAnyModel},
					Runtime: &corev1alpha1.AgentRuntimeSpec{
						Image:            "example/runtime:v1",
						Port:             4096,
						RuntimeClassName: "gvisor",
						ReadinessProbe: &corev1.Probe{
							ProbeHandler: corev1.ProbeHandler{
								HTTPGet: &corev1.HTTPGetAction{Path: "/ready", Port: intstr.FromInt32(4096)},
							},
						},
					},
					Workspace: &corev1alpha1.AgentWorkspaceSpec{Repository: "https://example.com/x.git"},
				},
			}
			Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, agent))).To(Succeed())
		})

		AfterEach(func() {
			deleteIfExists(ctx, agentKey, &corev1alpha1.Agent{})
			deleteIfExists(ctx, depKey, &appsv1.Deployment{})
			deleteIfExists(ctx, types.NamespacedName{Name: agentName + "-workspace", Namespace: nsDefault}, &corev1.PersistentVolumeClaim{})
		})

		It("uses the given probe and schedules onto the named runtime class", func() {
			reconciler := &AgentReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: agentKey})
			Expect(err).NotTo(HaveOccurred())

			dep := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, depKey, dep)).To(Succeed())

			By("preferring the Agent's own probe over the workspace default")
			probe := dep.Spec.Template.Spec.Containers[0].ReadinessProbe
			Expect(probe).NotTo(BeNil())
			Expect(probe.HTTPGet).NotTo(BeNil(), "an explicit probe must not be replaced by the default exec one")
			Expect(probe.HTTPGet.Path).To(Equal("/ready"))
			Expect(probe.Exec).To(BeNil())

			By("naming the RuntimeClass on the pod, not the container")
			Expect(dep.Spec.Template.Spec.RuntimeClassName).NotTo(BeNil())
			Expect(*dep.Spec.Template.Spec.RuntimeClassName).To(Equal("gvisor"))

			By("clearing the RuntimeClass when the Agent stops asking for one")
			// Removing the field has to take effect on an existing Deployment, or an
			// Agent could never be moved back off a sandbox it was pinned to.
			agent := &corev1alpha1.Agent{}
			Expect(k8sClient.Get(ctx, agentKey, agent)).To(Succeed())
			agent.Spec.Runtime.RuntimeClassName = ""
			Expect(k8sClient.Update(ctx, agent)).To(Succeed())
			_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: agentKey})
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, depKey, dep)).To(Succeed())
			Expect(dep.Spec.Template.Spec.RuntimeClassName).To(BeNil())
		})
	})

	Context("When an Agent names a ServiceAccount", func() {
		const agentName = "test-sa-agent"
		agentKey := types.NamespacedName{Name: agentName, Namespace: nsDefault}
		depKey := types.NamespacedName{Name: agentName + "-runtime", Namespace: nsDefault}

		BeforeEach(func() {
			agent := &corev1alpha1.Agent{
				ObjectMeta: metav1.ObjectMeta{Name: agentName, Namespace: nsDefault},
				Spec: corev1alpha1.AgentSpec{
					ModelRef: &corev1alpha1.LocalReference{Name: testAnyModel},
					Runtime: &corev1alpha1.AgentRuntimeSpec{
						Image:              testCodingImage,
						ServiceAccountName: "agent-runtime",
					},
				},
			}
			Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, agent))).To(Succeed())
		})

		AfterEach(func() {
			deleteIfExists(ctx, agentKey, &corev1alpha1.Agent{})
			deleteIfExists(ctx, depKey, &appsv1.Deployment{})
		})

		It("runs the runtime pods as that account", func() {
			reconciler := &AgentReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: agentKey})
			Expect(err).NotTo(HaveOccurred())

			dep := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, depKey, dep)).To(Succeed())
			Expect(dep.Spec.Template.Spec.ServiceAccountName).To(Equal("agent-runtime"))
		})
	})

	// An Agent with no workspace keeps the previous behavior exactly.
	Context("When an Agent has no workspace", func() {
		const agentName = "test-no-ws-agent"
		agentKey := types.NamespacedName{Name: agentName, Namespace: nsDefault}
		depKey := types.NamespacedName{Name: agentName + "-runtime", Namespace: nsDefault}

		BeforeEach(func() {
			agent := &corev1alpha1.Agent{
				ObjectMeta: metav1.ObjectMeta{Name: agentName, Namespace: nsDefault},
				Spec: corev1alpha1.AgentSpec{
					ModelRef: &corev1alpha1.LocalReference{Name: testAnyModel},
					Runtime:  &corev1alpha1.AgentRuntimeSpec{Image: "example/runtime:v1", Replicas: 2},
				},
			}
			Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, agent))).To(Succeed())
		})

		AfterEach(func() {
			deleteIfExists(ctx, agentKey, &corev1alpha1.Agent{})
			deleteIfExists(ctx, depKey, &appsv1.Deployment{})
		})

		It("creates no claim, no init container, and keeps its replica count", func() {
			reconciler := &AgentReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: agentKey})
			Expect(err).NotTo(HaveOccurred())

			dep := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, depKey, dep)).To(Succeed())
			Expect(dep.Spec.Template.Spec.InitContainers).To(BeEmpty())
			Expect(dep.Spec.Template.Spec.Volumes).To(BeEmpty())
			// The single-writer pinning is workspace-specific; a stateless runtime
			// keeps whatever replica count it asked for.
			Expect(*dep.Spec.Replicas).To(Equal(int32(2)))
			// No probe is imposed either. The default one asks for projected config,
			// which is a workspace-runtime concept; a runtime that just serves
			// /api/chat would be marked permanently unready by it.
			Expect(dep.Spec.Template.Spec.Containers[0].ReadinessProbe).To(BeNil())
			Expect(dep.Spec.Template.Spec.RuntimeClassName).To(BeNil())

			pvc := &corev1.PersistentVolumeClaim{}
			err = k8sClient.Get(ctx, types.NamespacedName{Name: agentName + "-workspace", Namespace: nsDefault}, pvc)
			Expect(err).To(HaveOccurred())
		})
	})
})

var _ = Describe("Agent peering", func() {
	ctx := context.Background()

	// Every Agent needs a resolvable Model, or it stops at ReferenceNotFound and
	// the peer checks below never run.
	const peerModel = "test-peer-model"
	BeforeEach(func() {
		model := &corev1alpha1.Model{
			ObjectMeta: metav1.ObjectMeta{Name: peerModel, Namespace: nsDefault},
			Spec:       corev1alpha1.ModelSpec{Provider: testProvider, ModelName: testModelName},
		}
		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, model))).To(Succeed())
	})

	Context("When an Agent exposes itself to peers", func() {
		const agentName = "test-peer-target"
		agentKey := types.NamespacedName{Name: agentName, Namespace: nsDefault}
		svcKey := types.NamespacedName{Name: agentName + "-peer", Namespace: nsDefault}

		BeforeEach(func() {
			agent := &corev1alpha1.Agent{
				ObjectMeta: metav1.ObjectMeta{Name: agentName, Namespace: nsDefault},
				Spec: corev1alpha1.AgentSpec{
					ModelRef: &corev1alpha1.LocalReference{Name: peerModel},
					Runtime:  &corev1alpha1.AgentRuntimeSpec{Image: testCodingImage, Port: 8080},
					Expose: &corev1alpha1.AgentExposeSpec{
						Port:        9000,
						Description: "Owns the payments API",
					},
				},
			}
			Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, agent))).To(Succeed())
		})

		AfterEach(func() {
			deleteIfExists(ctx, agentKey, &corev1alpha1.Agent{})
			deleteIfExists(ctx, svcKey, &corev1.Service{})
			deleteIfExists(ctx, types.NamespacedName{Name: agentName + "-runtime", Namespace: nsDefault}, &appsv1.Deployment{})
			deleteIfExists(ctx, types.NamespacedName{Name: agentName + "-runtime", Namespace: nsDefault}, &corev1.Service{})
		})

		It("publishes a peer Service and endpoint", func() {
			reconciler := &AgentReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: agentKey})
			Expect(err).NotTo(HaveOccurred())

			svc := &corev1.Service{}
			Expect(k8sClient.Get(ctx, svcKey, svc)).To(Succeed())
			Expect(svc.Spec.Ports[0].Port).To(Equal(int32(9000)))

			agent := &corev1alpha1.Agent{}
			Expect(k8sClient.Get(ctx, agentKey, agent)).To(Succeed())
			// The endpoint is what a peer's Tool resolves to, so it must be a
			// complete in-cluster address, not just a host.
			Expect(agent.Status.PeerEndpoint).To(Equal(
				"http://" + agentName + "-peer." + nsDefault + ".svc:9000"))
		})
	})

	// The whole point of peering: one repo's agent consults another's, and the
	// edge is an ordinary Tool so ToolPolicy governs it.
	Context("When one Agent consults another", func() {
		const (
			callerName = "test-peer-caller"
			targetName = "test-peer-callee"
			toolName   = "ask-callee"
		)
		callerKey := types.NamespacedName{Name: callerName, Namespace: nsDefault}

		BeforeEach(func() {
			target := &corev1alpha1.Agent{
				ObjectMeta: metav1.ObjectMeta{Name: targetName, Namespace: nsDefault},
				Spec: corev1alpha1.AgentSpec{
					ModelRef: &corev1alpha1.LocalReference{Name: peerModel},
					Runtime:  &corev1alpha1.AgentRuntimeSpec{Image: testCodingImage, Port: 8080},
					Expose:   &corev1alpha1.AgentExposeSpec{Description: "Owns the web frontend"},
				},
			}
			Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, target))).To(Succeed())

			tool := &corev1alpha1.Tool{
				ObjectMeta: metav1.ObjectMeta{Name: toolName, Namespace: nsDefault},
				Spec: corev1alpha1.ToolSpec{
					Type:     corev1alpha1.ToolTypeMCP,
					AgentRef: &corev1alpha1.LocalReference{Name: targetName},
				},
			}
			Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, tool))).To(Succeed())

			caller := repoAgent(callerName, "https://github.com/org/api", func(a *corev1alpha1.Agent) {
				a.Spec.ModelRef = &corev1alpha1.LocalReference{Name: peerModel}
				a.Spec.ToolRefs = []corev1alpha1.LocalReference{{Name: toolName}}
			})
			Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, caller))).To(Succeed())
		})

		AfterEach(func() {
			deleteIfExists(ctx, callerKey, &corev1alpha1.Agent{})
			deleteIfExists(ctx, types.NamespacedName{Name: targetName, Namespace: nsDefault}, &corev1alpha1.Agent{})
			deleteIfExists(ctx, types.NamespacedName{Name: toolName, Namespace: nsDefault}, &corev1alpha1.Tool{})
			for _, n := range []string{callerName, targetName} {
				deleteIfExists(ctx, types.NamespacedName{Name: n + "-runtime", Namespace: nsDefault}, &appsv1.Deployment{})
				deleteIfExists(ctx, types.NamespacedName{Name: n + "-runtime", Namespace: nsDefault}, &corev1.Service{})
				deleteIfExists(ctx, types.NamespacedName{Name: n + "-peer", Namespace: nsDefault}, &corev1.Service{})
				deleteIfExists(ctx, types.NamespacedName{Name: n + "-workspace", Namespace: nsDefault}, &corev1.PersistentVolumeClaim{})
			}
		})

		It("reaches Ready with the peer edge declared", func() {
			reconciler := &AgentReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			// The target must reconcile first so it publishes its endpoint.
			_, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: targetName, Namespace: nsDefault}})
			Expect(err).NotTo(HaveOccurred())

			_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: callerKey})
			Expect(err).NotTo(HaveOccurred())

			caller := &corev1alpha1.Agent{}
			Expect(k8sClient.Get(ctx, callerKey, caller)).To(Succeed())
			Expect(caller.Status.Phase).To(Equal(corev1alpha1.AgentPhaseReady))
		})
	})

	// A peer Tool that cannot resolve is a declaration error: the model would
	// advertise a tool whose every call fails.
	Context("When a peer Tool points at an unexposed Agent", func() {
		const (
			callerName = "test-peer-unexposed-caller"
			targetName = "test-peer-unexposed-callee"
			toolName   = "ask-unexposed"
		)
		callerKey := types.NamespacedName{Name: callerName, Namespace: nsDefault}

		BeforeEach(func() {
			// Deliberately no spec.expose.
			target := &corev1alpha1.Agent{
				ObjectMeta: metav1.ObjectMeta{Name: targetName, Namespace: nsDefault},
				Spec:       corev1alpha1.AgentSpec{ModelRef: &corev1alpha1.LocalReference{Name: peerModel}},
			}
			Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, target))).To(Succeed())

			tool := &corev1alpha1.Tool{
				ObjectMeta: metav1.ObjectMeta{Name: toolName, Namespace: nsDefault},
				Spec: corev1alpha1.ToolSpec{
					Type:     corev1alpha1.ToolTypeMCP,
					AgentRef: &corev1alpha1.LocalReference{Name: targetName},
				},
			}
			Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, tool))).To(Succeed())

			caller := &corev1alpha1.Agent{
				ObjectMeta: metav1.ObjectMeta{Name: callerName, Namespace: nsDefault},
				Spec: corev1alpha1.AgentSpec{
					ModelRef: &corev1alpha1.LocalReference{Name: peerModel},
					ToolRefs: []corev1alpha1.LocalReference{{Name: toolName}},
				},
			}
			Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, caller))).To(Succeed())
		})

		AfterEach(func() {
			deleteIfExists(ctx, callerKey, &corev1alpha1.Agent{})
			deleteIfExists(ctx, types.NamespacedName{Name: targetName, Namespace: nsDefault}, &corev1alpha1.Agent{})
			deleteIfExists(ctx, types.NamespacedName{Name: toolName, Namespace: nsDefault}, &corev1alpha1.Tool{})
		})

		It("marks the caller Degraded naming the unexposed target", func() {
			reconciler := &AgentReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: callerKey})
			Expect(err).NotTo(HaveOccurred())

			caller := &corev1alpha1.Agent{}
			Expect(k8sClient.Get(ctx, callerKey, caller)).To(Succeed())
			Expect(caller.Status.Phase).To(Equal(corev1alpha1.AgentPhaseDegraded))
			cond := meta.FindStatusCondition(caller.Status.Conditions, corev1alpha1.ConditionReady)
			Expect(cond.Reason).To(Equal(corev1alpha1.ReasonPolicyViolation))
			Expect(cond.Message).To(ContainSubstring(targetName))
			Expect(cond.Message).To(ContainSubstring("spec.expose"))
		})
	})

	// A self-referencing peer Tool is a loop with no purpose; catch it at apply
	// time rather than letting the agent call itself.
	Context("When a peer Tool points at the Agent itself", func() {
		const (
			agentName = "test-peer-self"
			toolName  = "ask-self"
		)
		agentKey := types.NamespacedName{Name: agentName, Namespace: nsDefault}

		BeforeEach(func() {
			tool := &corev1alpha1.Tool{
				ObjectMeta: metav1.ObjectMeta{Name: toolName, Namespace: nsDefault},
				Spec: corev1alpha1.ToolSpec{
					Type:     corev1alpha1.ToolTypeMCP,
					AgentRef: &corev1alpha1.LocalReference{Name: agentName},
				},
			}
			Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, tool))).To(Succeed())

			agent := &corev1alpha1.Agent{
				ObjectMeta: metav1.ObjectMeta{Name: agentName, Namespace: nsDefault},
				Spec: corev1alpha1.AgentSpec{
					ModelRef: &corev1alpha1.LocalReference{Name: peerModel},
					Runtime:  &corev1alpha1.AgentRuntimeSpec{Image: testCodingImage, Port: 8080},
					Expose:   &corev1alpha1.AgentExposeSpec{},
					ToolRefs: []corev1alpha1.LocalReference{{Name: toolName}},
				},
			}
			Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, agent))).To(Succeed())
		})

		AfterEach(func() {
			deleteIfExists(ctx, agentKey, &corev1alpha1.Agent{})
			deleteIfExists(ctx, types.NamespacedName{Name: toolName, Namespace: nsDefault}, &corev1alpha1.Tool{})
			deleteIfExists(ctx, types.NamespacedName{Name: agentName + "-runtime", Namespace: nsDefault}, &appsv1.Deployment{})
			deleteIfExists(ctx, types.NamespacedName{Name: agentName + "-runtime", Namespace: nsDefault}, &corev1.Service{})
			deleteIfExists(ctx, types.NamespacedName{Name: agentName + "-peer", Namespace: nsDefault}, &corev1.Service{})
		})

		It("marks the Agent Degraded", func() {
			reconciler := &AgentReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: agentKey})
			Expect(err).NotTo(HaveOccurred())

			agent := &corev1alpha1.Agent{}
			Expect(k8sClient.Get(ctx, agentKey, agent)).To(Succeed())
			Expect(agent.Status.Phase).To(Equal(corev1alpha1.AgentPhaseDegraded))
			cond := meta.FindStatusCondition(agent.Status.Conditions, corev1alpha1.ConditionReady)
			Expect(cond.Message).To(ContainSubstring("itself"))
		})
	})

	// The isolation guarantee: a peer Tool is an ordinary Tool, so denying it in
	// a ToolPolicy severs the cross-repository link with no separate
	// agent-to-agent authorization model.
	Context("When a ToolPolicy denies the peer Tool", func() {
		const (
			callerName = "test-peer-denied-caller"
			targetName = "test-peer-denied-callee"
			toolName   = "ask-denied"
			tpName     = "no-cross-repo"
		)
		callerKey := types.NamespacedName{Name: callerName, Namespace: nsDefault}

		BeforeEach(func() {
			target := &corev1alpha1.Agent{
				ObjectMeta: metav1.ObjectMeta{Name: targetName, Namespace: nsDefault},
				Spec: corev1alpha1.AgentSpec{
					ModelRef: &corev1alpha1.LocalReference{Name: peerModel},
					Runtime:  &corev1alpha1.AgentRuntimeSpec{Image: testCodingImage, Port: 8080},
					Expose:   &corev1alpha1.AgentExposeSpec{},
				},
			}
			Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, target))).To(Succeed())

			tool := &corev1alpha1.Tool{
				ObjectMeta: metav1.ObjectMeta{Name: toolName, Namespace: nsDefault},
				Spec: corev1alpha1.ToolSpec{
					Type:     corev1alpha1.ToolTypeMCP,
					AgentRef: &corev1alpha1.LocalReference{Name: targetName},
				},
			}
			Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, tool))).To(Succeed())

			tp := &corev1alpha1.ToolPolicy{
				ObjectMeta: metav1.ObjectMeta{Name: tpName, Namespace: nsDefault},
				Spec: corev1alpha1.ToolPolicySpec{
					Rules: []corev1alpha1.ToolRule{{Tool: toolName, Action: corev1alpha1.ToolActionDeny}},
				},
			}
			Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, tp))).To(Succeed())

			caller := &corev1alpha1.Agent{
				ObjectMeta: metav1.ObjectMeta{Name: callerName, Namespace: nsDefault},
				Spec: corev1alpha1.AgentSpec{
					ModelRef:       &corev1alpha1.LocalReference{Name: peerModel},
					ToolRefs:       []corev1alpha1.LocalReference{{Name: toolName}},
					ToolPolicyRefs: []corev1alpha1.LocalReference{{Name: tpName}},
				},
			}
			Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, caller))).To(Succeed())
		})

		AfterEach(func() {
			deleteIfExists(ctx, callerKey, &corev1alpha1.Agent{})
			deleteIfExists(ctx, types.NamespacedName{Name: targetName, Namespace: nsDefault}, &corev1alpha1.Agent{})
			deleteIfExists(ctx, types.NamespacedName{Name: toolName, Namespace: nsDefault}, &corev1alpha1.Tool{})
			deleteIfExists(ctx, types.NamespacedName{Name: tpName, Namespace: nsDefault}, &corev1alpha1.ToolPolicy{})
			deleteIfExists(ctx, types.NamespacedName{Name: targetName + "-runtime", Namespace: nsDefault}, &appsv1.Deployment{})
			deleteIfExists(ctx, types.NamespacedName{Name: targetName + "-runtime", Namespace: nsDefault}, &corev1.Service{})
			deleteIfExists(ctx, types.NamespacedName{Name: targetName + "-peer", Namespace: nsDefault}, &corev1.Service{})
		})

		It("severs the link by refusing the caller", func() {
			reconciler := &AgentReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: callerKey})
			Expect(err).NotTo(HaveOccurred())

			caller := &corev1alpha1.Agent{}
			Expect(k8sClient.Get(ctx, callerKey, caller)).To(Succeed())
			Expect(caller.Status.Phase).To(Equal(corev1alpha1.AgentPhaseDegraded))
			cond := meta.FindStatusCondition(caller.Status.Conditions, corev1alpha1.ConditionReady)
			Expect(cond.Reason).To(Equal(corev1alpha1.ReasonPolicyViolation))
			Expect(cond.Message).To(ContainSubstring(toolName))
		})
	})
})
