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

// Command demo provisions a complete CogNet Agent entirely in code (no YAML,
// no kubectl), waits for it to become Ready, port-forwards to the in-cluster
// MCP server in-process, then runs the agent loop — a real model making a real
// MCP tool call. It is a verification harness that doubles as an example of
// using the CogNet API types as a Go library.
//
// Prereqs: the operator is deployed and running, Docker image
// cognet-example-mcp:dev exists, and an LLM credential is in the environment
// (ANTHROPIC_BASE_URL+ANTHROPIC_AUTH_TOKEN, or OPENROUTER_API_KEY).
package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/hkmdxlftjf/agent-plane/api/v1alpha1"
	"github.com/hkmdxlftjf/agent-plane/internal/agentloop"
)

const ns = "cognet-demo-code"

var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(v1alpha1.AddToScheme(scheme))
}

func main() {
	prompt := "What is the delivery status of order A-42? Give me the carrier and ETA."
	if len(os.Args) > 1 {
		prompt = os.Args[1]
	}
	ctx := context.Background()

	provider, endpoint, model, apiKey, err := detectModel()
	if err != nil {
		fatal("detect model backend", err)
	}
	fmt.Printf("▶ model backend: %s %s (%s)\n", provider, model, endpoint)

	restCfg, err := ctrl.GetConfig()
	if err != nil {
		fatal("load kubeconfig", err)
	}
	k, err := client.New(restCfg, client.Options{Scheme: scheme})
	if err != nil {
		fatal("build client", err)
	}
	cs, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		fatal("build clientset", err)
	}

	// Always clean up the namespace on exit.
	defer func() {
		_ = k.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		fmt.Printf("\n▶ cleaned up namespace %s\n", ns)
	}()

	// 1. Provision everything in code.
	fmt.Println("\n▶ Provisioning CogNet resources (in code)")
	if err := provision(ctx, k, provider, endpoint, model, apiKey); err != nil {
		fatal("provision", err)
	}

	// 2. Wait for the Agent to be Ready and an MCP pod to be Running.
	fmt.Println("\n▶ Waiting for Agent Ready + MCP pod Running")
	if err := waitAgentReady(ctx, k); err != nil {
		fatal("wait agent ready", err)
	}
	pod, err := waitMCPPod(ctx, cs)
	if err != nil {
		fatal("wait mcp pod", err)
	}
	fmt.Printf("  agent Ready; mcp pod=%s\n", pod)

	// 3. Port-forward to the MCP server in-process (no external kubectl).
	fmt.Println("\n▶ Port-forwarding to the in-cluster MCP server")
	stop, err := portForward(restCfg, cs, pod, 18080, 8080)
	if err != nil {
		fatal("port-forward", err)
	}
	defer stop()

	// 4. Resolve the system prompt from the PromptTemplate (via the client).
	var pt v1alpha1.PromptTemplate
	_ = k.Get(ctx, client.ObjectKey{Namespace: ns, Name: "support-prompt"}, &pt)

	// 5. Run the agent loop against the real model + MCP tool.
	fmt.Printf("\n▶ Running agent loop (prompt: %q)\n", prompt)
	answer, err := agentloop.Run(ctx, agentloop.Config{
		Endpoint: endpoint,
		APIKey:   apiKey,
		Model:    model,
		System:   pt.Spec.System,
		Prompt:   prompt,
		Tools: []agentloop.Tool{{
			Name:        "order-lookup",
			Type:        "mcp",
			Description: "Look up the delivery status of a customer order by its order id.",
			Endpoint:    "http://localhost:18080",
			MCPToolName: "get_order_status",
			InputSchema: []byte(`{"type":"object","properties":{"orderId":{"type":"string"}},"required":["orderId"]}`),
		}},
		Logf: func(f string, a ...any) { fmt.Printf("  "+f+"\n", a...) },
	})
	if err != nil {
		fatal("agent loop", err)
	}
	fmt.Printf("\n✅ Final answer:\n%s\n", answer)
}

// provision creates the namespace, secret, and all CogNet CRs using the typed
// API — the code equivalent of `kubectl apply -k config/demo`.
func provision(ctx context.Context, k client.Client, provider, endpoint, model, apiKey string) error {
	int32p := func(i int32) *int32 { return &i }
	objs := []client.Object{
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}},
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "llm-secret", Namespace: ns},
			StringData: map[string]string{"api-key": apiKey},
		},
		&v1alpha1.Credential{
			ObjectMeta: metav1.ObjectMeta{Name: "llm-cred", Namespace: ns},
			Spec:       v1alpha1.CredentialSpec{SecretRef: v1alpha1.SecretKeyReference{Name: "llm-secret", Key: "api-key"}},
		},
		&v1alpha1.Model{
			ObjectMeta: metav1.ObjectMeta{Name: "llm-model", Namespace: ns},
			Spec: v1alpha1.ModelSpec{
				Provider: v1alpha1.ModelProvider(provider), ModelName: model, Endpoint: endpoint,
				CredentialRef: &v1alpha1.LocalReference{Name: "llm-cred"},
			},
		},
		&v1alpha1.MCPServer{
			ObjectMeta: metav1.ObjectMeta{Name: "orders-mcp", Namespace: ns},
			Spec:       v1alpha1.MCPServerSpec{Image: "cognet-example-mcp:dev", Transport: "http", Port: 8080, Replicas: 1},
		},
		&v1alpha1.Tool{
			ObjectMeta: metav1.ObjectMeta{Name: "order-lookup", Namespace: ns},
			Spec: v1alpha1.ToolSpec{
				Type: "mcp", Description: "Look up the delivery status of a customer order by its order id.",
				MCPServerRef: &v1alpha1.LocalReference{Name: "orders-mcp"}, MCPToolName: "get_order_status",
				TimeoutSeconds: int32p(15),
			},
		},
		&v1alpha1.PromptTemplate{
			ObjectMeta: metav1.ObjectMeta{Name: "support-prompt", Namespace: ns},
			Spec: v1alpha1.PromptTemplateSpec{Version: "1.0.0", System: "You are a concise customer-support agent. " +
				"When asked about an order, use the available tool to look it up, then answer with the concrete facts."},
		},
		&v1alpha1.Agent{
			ObjectMeta: metav1.ObjectMeta{Name: "support-agent", Namespace: ns},
			Spec: v1alpha1.AgentSpec{
				Description: "Demo agent provisioned entirely in Go.",
				ModelRef:    v1alpha1.LocalReference{Name: "llm-model"},
				PromptRef:   &v1alpha1.LocalReference{Name: "support-prompt"},
				ToolRefs:    []v1alpha1.LocalReference{{Name: "order-lookup"}},
			},
		},
	}
	for _, o := range objs {
		if err := k.Create(ctx, o); err != nil && !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("create %T %s: %w", o, o.GetName(), err)
		}
		fmt.Printf("  created %T/%s\n", o, o.GetName())
	}
	return nil
}

func waitAgentReady(ctx context.Context, k client.Client) error {
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		var a v1alpha1.Agent
		if err := k.Get(ctx, client.ObjectKey{Namespace: ns, Name: "support-agent"}, &a); err == nil {
			if a.Status.Phase == v1alpha1.AgentPhaseReady {
				return nil
			}
		}
		time.Sleep(2 * time.Second)
	}
	return fmt.Errorf("timeout waiting for agent Ready")
}

func waitMCPPod(ctx context.Context, cs *kubernetes.Clientset) (string, error) {
	deadline := time.Now().Add(120 * time.Second)
	for time.Now().Before(deadline) {
		// Ensure the Deployment reports availability first.
		dep, err := cs.AppsV1().Deployments(ns).Get(ctx, "orders-mcp", metav1.GetOptions{})
		if err == nil && dep.Status.AvailableReplicas > 0 {
			pods, err := cs.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{
				LabelSelector: "app.kubernetes.io/instance=orders-mcp",
			})
			if err == nil {
				for i := range pods.Items {
					if pods.Items[i].Status.Phase == corev1.PodRunning {
						return pods.Items[i].Name, nil
					}
				}
			}
		}
		time.Sleep(2 * time.Second)
	}
	return "", fmt.Errorf("timeout waiting for MCP pod")
}

// portForward opens an in-process port-forward to a pod (like `kubectl
// port-forward`, but no external process).
func portForward(restCfg *rest.Config, cs *kubernetes.Clientset, pod string, local, remote int) (func(), error) {
	reqURL := cs.CoreV1().RESTClient().Post().
		Resource("pods").Namespace(ns).Name(pod).SubResource("portforward").URL()
	transport, upgrader, err := spdy.RoundTripperFor(restCfg)
	if err != nil {
		return nil, err
	}
	dialer := spdy.NewDialer(upgrader, &http.Client{Transport: transport}, http.MethodPost, reqURL)
	stopCh := make(chan struct{})
	readyCh := make(chan struct{})
	fw, err := portforward.New(dialer, []string{fmt.Sprintf("%d:%d", local, remote)}, stopCh, readyCh, io.Discard, os.Stderr)
	if err != nil {
		return nil, err
	}
	errCh := make(chan error, 1)
	go func() { errCh <- fw.ForwardPorts() }()
	select {
	case <-readyCh:
		return func() { close(stopCh) }, nil
	case err := <-errCh:
		return nil, err
	case <-time.After(15 * time.Second):
		close(stopCh)
		return nil, fmt.Errorf("timeout establishing port-forward")
	}
}

func detectModel() (provider, endpoint, model, apiKey string, err error) {
	if base := os.Getenv("ANTHROPIC_BASE_URL"); base != "" && os.Getenv("ANTHROPIC_AUTH_TOKEN") != "" {
		m := os.Getenv("DEMO_MODEL")
		if m == "" {
			m = "claude-haiku-4-5-20251001"
		}
		return "custom", strings.TrimRight(base, "/") + "/v1", m, os.Getenv("ANTHROPIC_AUTH_TOKEN"), nil
	}
	if key := os.Getenv("OPENROUTER_API_KEY"); key != "" {
		m := os.Getenv("DEMO_MODEL")
		if m == "" {
			m = "openai/gpt-4o-mini"
		}
		return "openrouter", "https://openrouter.ai/api/v1", m, key, nil
	}
	return "", "", "", "", fmt.Errorf("set ANTHROPIC_BASE_URL+ANTHROPIC_AUTH_TOKEN or OPENROUTER_API_KEY")
}

func fatal(what string, err error) {
	fmt.Fprintf(os.Stderr, "✖ %s: %v\n", what, err)
	os.Exit(1)
}
