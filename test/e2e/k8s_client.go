package e2e

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextensionsclient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer/yaml"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/remotecommand"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayclient "sigs.k8s.io/gateway-api/pkg/client/clientset/versioned"
)

// K8sClient wraps Kubernetes client operations
type K8sClient struct {
	clientset     *kubernetes.Clientset
	dynamic       dynamic.Interface
	gatewayClient gatewayclient.Interface
	config        *rest.Config
	logger        testLogger
}

// NewK8sClient creates a new Kubernetes client from kubeconfig
func NewK8sClient(kubeconfigPath string) (*K8sClient, error) {
	config, err := clientcmd.BuildConfigFromFlags("", kubeconfigPath)
	if err != nil {
		return nil, fmt.Errorf("failed to build config from kubeconfig: %w", err)
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("failed to create clientset: %w", err)
	}

	dynamicClient, err := dynamic.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("failed to create dynamic client: %w", err)
	}

	gatewayClient, err := gatewayclient.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("failed to create gateway client: %w", err)
	}

	return &K8sClient{
		clientset:     clientset,
		dynamic:       dynamicClient,
		gatewayClient: gatewayClient,
		config:        config,
		logger:        defaultLogger,
	}, nil
}

// SetLogger sets a custom logger for the client
func (k *K8sClient) SetLogger(logger testLogger) {
	k.logger = logger
}

// CreateNamespace creates a namespace if it doesn't exist
func (k *K8sClient) CreateNamespace(ctx context.Context, name string) error {
	k.logger.Logf("[K8sClient] Creating namespace: %s", name)
	_, err := k.clientset.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
	}, metav1.CreateOptions{})
	if err != nil {
		// Ignore if namespace already exists
		if !errors.IsAlreadyExists(err) {
			return fmt.Errorf("failed to create namespace: %w", err)
		}
		k.logger.Logf("[K8sClient] Namespace %s already exists", name)
	} else {
		k.logger.Logf("[K8sClient] Namespace %s created", name)
	}
	return nil
}

// WaitForPodsReady waits for pods matching the label selector to be ready
func (k *K8sClient) WaitForPodsReady(ctx context.Context, namespace, labelSelector string, timeout time.Duration) error {
	k.logger.Logf("[K8sClient] Waiting for pods %s/%s (timeout: %v)", namespace, labelSelector, timeout)
	selector, err := labels.Parse(labelSelector)
	if err != nil {
		return fmt.Errorf("failed to parse label selector: %w", err)
	}

	attempt := 0
	foundPods := false
	err = wait.PollUntilContextTimeout(ctx, TestPollInterval, timeout, true, func(ctx context.Context) (bool, error) {
		// Check if context is cancelled
		if ctx.Err() != nil {
			return false, ctx.Err()
		}

		attempt++
		pods, err := k.clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
			LabelSelector: selector.String(),
		})
		if err != nil {
			// Check if it's a context error - if so, return it to stop polling
			if ctx.Err() != nil {
				return false, ctx.Err()
			}
			// For other errors, continue polling (they might be transient)
			return false, nil
		}

		if len(pods.Items) == 0 {
			if attempt%15 == 0 { // Log every 15 attempts (~30s)
				k.logger.Logf("[K8sClient] Waiting for pods... (attempt %d)", attempt)
			}
			return false, nil
		}

		if !foundPods {
			k.logger.Logf("[K8sClient] Found %d pod(s)", len(pods.Items))
			foundPods = true
		}

		// Check if all pods are ready
		allReady := true
		for _, pod := range pods.Items {
			if pod.Status.Phase != corev1.PodRunning {
				allReady = false
				continue
			}
			ready := false
			for _, condition := range pod.Status.Conditions {
				if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
					ready = true
					break
				}
			}
			if !ready {
				allReady = false
			}
		}

		if allReady {
			k.logger.Logf("[K8sClient] All pods ready")
		}
		return allReady, nil
	})

	// If we timed out, extract logs for debugging
	if err != nil {
		k.logger.Logf("[K8sClient] Timeout after %d attempts: %v", attempt, err)
		if ctx.Err() == context.DeadlineExceeded || err == context.DeadlineExceeded {
			k.logger.Log("[K8sClient] Extracting pod logs for debugging...")
			// Use a background context with a short timeout for log extraction
			// This ensures we can still extract logs even if the original context timed out
			logCtx, logCancel := context.WithTimeout(context.Background(), LogExtractionTimeout)
			defer logCancel()

			// Extract logs from pods that aren't ready
			pods, listErr := k.clientset.CoreV1().Pods(namespace).List(logCtx, metav1.ListOptions{
				LabelSelector: selector.String(),
			})
			if listErr == nil {
				for _, pod := range pods.Items {
					k.logger.Logf("\n[K8sClient] === Pod %s/%s Status ===", namespace, pod.Name)
					k.logger.Logf("  Phase: %s", pod.Status.Phase)
					k.logger.Logf("  PodIP: %s", pod.Status.PodIP)
					k.logger.Logf("  HostIP: %s", pod.Status.HostIP)

					// Print container statuses
					for _, cs := range pod.Status.ContainerStatuses {
						k.logger.Logf("  Container %s: Ready=%v, RestartCount=%d", cs.Name, cs.Ready, cs.RestartCount)
						if cs.State.Waiting != nil {
							k.logger.Logf("    Waiting: Reason=%s, Message=%s", cs.State.Waiting.Reason, cs.State.Waiting.Message)
						}
						if cs.State.Terminated != nil {
							k.logger.Logf("    Terminated: Reason=%s, ExitCode=%d, Message=%s",
								cs.State.Terminated.Reason, cs.State.Terminated.ExitCode, cs.State.Terminated.Message)
						}
					}

					// Print conditions
					for _, condition := range pod.Status.Conditions {
						k.logger.Logf("  Condition %s: Status=%s, Reason=%s, Message=%s",
							condition.Type, condition.Status, condition.Reason, condition.Message)
					}

					if pod.Status.Phase != corev1.PodRunning {
						logs, logErr := k.GetPodLogs(logCtx, namespace, pod.Name, 100)
						if logErr == nil {
							k.logger.Logf("\n[K8sClient] === Pod %s/%s logs (not ready, phase: %s) ===", namespace, pod.Name, pod.Status.Phase)
							k.logger.Log(logs)
							k.logger.Log("[K8sClient] === End of logs ===")
						} else {
							k.logger.Logf("[K8sClient] Failed to get logs for pod %s/%s: %v", namespace, pod.Name, logErr)
						}
					} else {
						// Check if pod is actually ready
						ready := false
						for _, condition := range pod.Status.Conditions {
							if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
								ready = true
								break
							}
						}
						if !ready {
							logs, logErr := k.GetPodLogs(logCtx, namespace, pod.Name, 100)
							if logErr == nil {
								k.logger.Logf("\n[K8sClient] === Pod %s/%s logs (not ready) ===", namespace, pod.Name)
								k.logger.Log(logs)
								k.logger.Log("[K8sClient] === End of logs ===")
							} else {
								k.logger.Logf("[K8sClient] Failed to get logs for pod %s/%s: %v", namespace, pod.Name, logErr)
							}
						}
					}

					// Get events for the pod
					events, eventErr := k.clientset.CoreV1().Events(namespace).List(logCtx, metav1.ListOptions{
						FieldSelector: fmt.Sprintf("involvedObject.name=%s", pod.Name),
					})
					if eventErr == nil && len(events.Items) > 0 {
						k.logger.Logf("\n[K8sClient] === Pod %s/%s Events ===", namespace, pod.Name)
						for _, event := range events.Items {
							k.logger.Logf("  %s: %s - %s", event.LastTimestamp.Format(time.RFC3339), event.Reason, event.Message)
						}
						k.logger.Log("[K8sClient] === End of events ===")
					}
				}
			} else {
				k.logger.Logf("[K8sClient] Failed to list pods for log extraction: %v", listErr)
			}
		}
	} else {
		k.logger.Logf("[K8sClient] All pods are ready in namespace %s", namespace)
	}

	return err
}

// GetPodLogs retrieves logs from a pod
func (k *K8sClient) GetPodLogs(ctx context.Context, namespace, podName string, tailLines int64) (string, error) {
	req := k.clientset.CoreV1().Pods(namespace).GetLogs(podName, &corev1.PodLogOptions{
		TailLines: &tailLines,
	})
	logs, err := req.Stream(ctx)
	if err != nil {
		return "", fmt.Errorf("failed to stream logs: %w", err)
	}
	defer logs.Close()

	logOutput, err := io.ReadAll(logs)
	if err != nil {
		return "", fmt.Errorf("failed to read logs: %w", err)
	}

	return string(logOutput), nil
}

// GetServiceClusterIP gets the cluster IP of a service
func (k *K8sClient) GetServiceClusterIP(ctx context.Context, namespace, name string) (string, error) {
	svc, err := k.clientset.CoreV1().Services(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("failed to get service: %w", err)
	}
	return svc.Spec.ClusterIP, nil
}

// ApplyManifest applies a Kubernetes manifest YAML using server-side apply
func (k *K8sClient) ApplyManifest(ctx context.Context, manifest string) error {
	// Parse YAML and apply using server-side apply
	// This is a simplified version - for production, use sigs.k8s.io/yaml and proper object decoding
	return fmt.Errorf("not implemented - use ApplyYAML or create resources directly")
}

// ApplyYAML applies YAML resources using server-side apply
func (k *K8sClient) ApplyYAML(ctx context.Context, yamlContent string) error {
	decoder := yaml.NewDecodingSerializer(unstructured.UnstructuredJSONScheme)

	// Split YAML by --- separator
	manifests := strings.Split(yamlContent, "---")

	for _, manifest := range manifests {
		manifest = strings.TrimSpace(manifest)
		if manifest == "" {
			continue
		}

		// Decode YAML to unstructured object
		obj := &unstructured.Unstructured{}
		_, gvk, err := decoder.Decode([]byte(manifest), nil, obj)
		if err != nil {
			return fmt.Errorf("failed to decode YAML: %w", err)
		}

		// Get the appropriate client for the resource type
		if err := k.applyUnstructured(ctx, obj, gvk); err != nil {
			return fmt.Errorf("failed to apply resource %s/%s: %w", gvk.Kind, obj.GetName(), err)
		}
	}

	return nil
}

// applyUnstructured applies an unstructured object using server-side apply
func (k *K8sClient) applyUnstructured(ctx context.Context, obj *unstructured.Unstructured, gvk *schema.GroupVersionKind) error {
	// For now, use a simple create-or-update approach
	// In production, you'd use server-side apply with proper field managers

	// Remove namespace from cluster-scoped resources
	clusterScopedKinds := map[string]bool{
		"EnvoyGateway":       true,
		"GatewayClass":       true,
		"ClusterRole":        true,
		"ClusterRoleBinding": true,
		"Namespace":          true,
		"Node":               true,
		"PersistentVolume":   true,
	}
	if clusterScopedKinds[gvk.Kind] {
		obj.SetNamespace("")
	}

	// Get the resource client based on GVK
	// This is simplified - in production, use dynamic client or specific typed clients
	namespace := obj.GetNamespace()
	if namespace == "" && !clusterScopedKinds[gvk.Kind] {
		namespace = "default"
	}

	// Try to get existing resource
	_, err := k.getUnstructuredResource(ctx, obj, gvk)
	if errors.IsNotFound(err) {
		// Create if not exists
		return k.createUnstructuredResource(ctx, obj, gvk)
	} else if err != nil {
		return fmt.Errorf("failed to check if resource exists: %w", err)
	}

	// Update if exists
	return k.updateUnstructuredResource(ctx, obj, gvk)
}

// Helper methods for unstructured resources using dynamic client
func (k *K8sClient) getUnstructuredResource(ctx context.Context, obj *unstructured.Unstructured, gvk *schema.GroupVersionKind) (*unstructured.Unstructured, error) {
	gvr := k.getGVR(gvk)
	resourceClient := k.getResourceClient(gvr, gvk.Kind, obj.GetNamespace())
	return resourceClient.Get(ctx, obj.GetName(), metav1.GetOptions{})
}

func (k *K8sClient) createUnstructuredResource(ctx context.Context, obj *unstructured.Unstructured, gvk *schema.GroupVersionKind) error {
	gvr := k.getGVR(gvk)
	resourceClient := k.getResourceClient(gvr, gvk.Kind, obj.GetNamespace())
	_, err := resourceClient.Create(ctx, obj, metav1.CreateOptions{})
	if err != nil {
		// Provide more context about the error
		return fmt.Errorf("failed to create %s/%s (GVR: %s/%s/%s, namespace: %s): %w",
			gvk.Kind, obj.GetName(), gvr.Group, gvr.Version, gvr.Resource, obj.GetNamespace(), err)
	}
	return nil
}

func (k *K8sClient) updateUnstructuredResource(ctx context.Context, obj *unstructured.Unstructured, gvk *schema.GroupVersionKind) error {
	gvr := k.getGVR(gvk)
	resourceClient := k.getResourceClient(gvr, gvk.Kind, obj.GetNamespace())

	_, err := resourceClient.Apply(ctx, obj.GetName(), obj, metav1.ApplyOptions{
		FieldManager: "xds-backend-e2e-test",
	}, "")
	return err
}

// getResourceClient returns the appropriate resource client (namespaced or cluster-scoped)
func (k *K8sClient) getResourceClient(gvr schema.GroupVersionResource, kind, namespace string) dynamic.ResourceInterface {
	baseClient := k.dynamic.Resource(gvr)

	// Cluster-scoped resources
	clusterScopedKinds := map[string]bool{
		"EnvoyGateway":       true,
		"GatewayClass":       true,
		"ClusterRole":        true,
		"ClusterRoleBinding": true,
		"Namespace":          true,
		"Node":               true,
		"PersistentVolume":   true,
	}
	if clusterScopedKinds[kind] {
		return baseClient
	}

	// Namespaced resources
	if namespace == "" {
		namespace = "default"
	}
	return baseClient.Namespace(namespace)
}

// getGVR converts GroupVersionKind to GroupVersionResource with proper resource name
func (k *K8sClient) getGVR(gvk *schema.GroupVersionKind) schema.GroupVersionResource {
	gvr := schema.GroupVersionResource{
		Group:    gvk.Group,
		Version:  gvk.Version,
		Resource: strings.ToLower(gvk.Kind) + "s", // Simple pluralization
	}

	// Handle special cases for resource names
	switch gvk.Kind {
	case "EnvoyGateway":
		// Envoy Gateway uses "envoygateways" as the resource name
		gvr.Resource = "envoygateways"
	case "EnvoyProxy":
		// EnvoyProxy uses "envoyproxies" as the resource name
		gvr.Resource = "envoyproxies"
	case "XdsBackend":
		// XdsBackend uses "xdsbackends" as the resource name
		gvr.Resource = "xdsbackends"
	case "Gateway":
		// Gateway API uses "gateways" as the resource name
		gvr.Resource = "gateways"
	case "GatewayClass":
		// Gateway API uses "gatewayclasses" as the resource name
		gvr.Resource = "gatewayclasses"
	case "HTTPRoute":
		// Gateway API uses "httproutes" as the resource name
		gvr.Resource = "httproutes"
	}

	return gvr
}

// WaitForCRD waits for a CRD to be installed and established
func (k *K8sClient) WaitForCRD(ctx context.Context, crdName string) error {
	apiextensionsClient, err := apiextensionsclient.NewForConfig(k.config)
	if err != nil {
		return fmt.Errorf("failed to create apiextensions client: %w", err)
	}

	timeoutCtx, cancel := context.WithTimeout(ctx, TestTimeout)
	defer cancel()

	err = wait.PollUntilContextTimeout(timeoutCtx, TestPollInterval, TestTimeout, true, func(ctx context.Context) (bool, error) {
		crd, err := apiextensionsClient.ApiextensionsV1().CustomResourceDefinitions().Get(ctx, crdName, metav1.GetOptions{})
		if err != nil {
			if errors.IsNotFound(err) {
				return false, nil // CRD not found yet, continue polling
			}
			return false, err // Other error, stop polling
		}

		// Check if CRD is established
		for _, condition := range crd.Status.Conditions {
			if condition.Type == apiextensionsv1.Established && condition.Status == apiextensionsv1.ConditionTrue {
				return true, nil // CRD is established
			}
		}

		return false, nil // CRD exists but not established yet
	})

	if err != nil {
		return fmt.Errorf("CRD %s not available: %w", crdName, err)
	}

	return nil
}

// CreateGateway creates a Gateway resource using the Gateway API client
func (k *K8sClient) CreateGateway(ctx context.Context, namespace string, gateway *gatewayv1.Gateway) (*gatewayv1.Gateway, error) {
	return k.gatewayClient.GatewayV1().Gateways(namespace).Create(ctx, gateway, metav1.CreateOptions{})
}

// GetGateway gets a Gateway resource
func (k *K8sClient) GetGateway(ctx context.Context, namespace, name string) (*gatewayv1.Gateway, error) {
	return k.gatewayClient.GatewayV1().Gateways(namespace).Get(ctx, name, metav1.GetOptions{})
}

// GetGatewayClass gets a GatewayClass resource
func (k *K8sClient) GetGatewayClass(ctx context.Context, name string) (*gatewayv1.GatewayClass, error) {
	return k.gatewayClient.GatewayV1().GatewayClasses().Get(ctx, name, metav1.GetOptions{})
}

// GetGatewayClient returns the Gateway API client
func (k *K8sClient) GetGatewayClient() gatewayclient.Interface {
	return k.gatewayClient
}

// CreateHTTPRoute creates an HTTPRoute resource using the Gateway API client
func (k *K8sClient) CreateHTTPRoute(ctx context.Context, namespace string, route *gatewayv1.HTTPRoute) (*gatewayv1.HTTPRoute, error) {
	return k.gatewayClient.GatewayV1().HTTPRoutes(namespace).Create(ctx, route, metav1.CreateOptions{})
}

// GetHTTPRoute gets an HTTPRoute resource
func (k *K8sClient) GetHTTPRoute(ctx context.Context, namespace, name string) (*gatewayv1.HTTPRoute, error) {
	return k.gatewayClient.GatewayV1().HTTPRoutes(namespace).Get(ctx, name, metav1.GetOptions{})
}

// WaitForGatewayReady waits for a Gateway to be accepted and have listeners ready
func (k *K8sClient) WaitForGatewayReady(ctx context.Context, namespace, name string, timeout time.Duration) error {
	k.logger.Logf("[K8sClient] Waiting for Gateway %s/%s to be ready (timeout: %v)...", namespace, name, timeout)
	timeoutCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	attempt := 0
	return wait.PollUntilContextTimeout(timeoutCtx, TestPollInterval, timeout, true, func(ctx context.Context) (bool, error) {
		attempt++
		gateway, err := k.GetGateway(ctx, namespace, name)
		if err != nil {
			if errors.IsNotFound(err) {
				k.logger.Logf("[K8sClient] Gateway %s/%s not found (attempt %d)", namespace, name, attempt)
				return false, nil
			}
			k.logger.Logf("[K8sClient] Error getting Gateway %s/%s: %v", namespace, name, err)
			return false, err
		}

		// Log Gateway spec for debugging
		k.logger.Logf("[K8sClient] Gateway %s/%s: GatewayClassName=%s, Listeners=%d (attempt %d)",
			namespace, name, gateway.Spec.GatewayClassName, len(gateway.Spec.Listeners), attempt)

		// Log Gateway status for debugging
		if len(gateway.Status.Conditions) > 0 {
			for _, condition := range gateway.Status.Conditions {
				k.logger.Logf("[K8sClient] Gateway %s/%s condition: %s=%s (reason: %s, message: %s)",
					namespace, name, condition.Type, condition.Status, condition.Reason, condition.Message)
			}
		} else {
			k.logger.Logf("[K8sClient] Gateway %s/%s has no status conditions yet (attempt %d)", namespace, name, attempt)
		}

		// Log listener status
		if len(gateway.Status.Listeners) > 0 {
			for _, listener := range gateway.Status.Listeners {
				k.logger.Logf("[K8sClient] Gateway %s/%s listener %s: AttachedRoutes=%d, Conditions=%d",
					namespace, name, listener.Name, listener.AttachedRoutes, len(listener.Conditions))
				for _, listenerCondition := range listener.Conditions {
					k.logger.Logf("[K8sClient] Gateway %s/%s listener %s condition: %s=%s (reason: %s)",
						namespace, name, listener.Name, listenerCondition.Type, listenerCondition.Status)
				}
			}
		} else {
			k.logger.Logf("[K8sClient] Gateway %s/%s has no listener status yet (attempt %d)", namespace, name, attempt)
		}

		// Check if Gateway is accepted
		gatewayAccepted := false
		for _, condition := range gateway.Status.Conditions {
			if condition.Type == string(gatewayv1.GatewayConditionAccepted) {
				if condition.Status == metav1.ConditionTrue {
					gatewayAccepted = true
					k.logger.Logf("[K8sClient] Gateway %s/%s is ACCEPTED", namespace, name)
				} else {
					k.logger.Logf("[K8sClient] Gateway %s/%s is NOT accepted: status=%s, reason=%s, message=%s",
						namespace, name, condition.Status, condition.Reason, condition.Message)
				}
			}
		}

		if !gatewayAccepted {
			k.logger.Logf("[K8sClient] Gateway %s/%s not accepted yet, continuing to wait...", namespace, name)
			return false, nil
		}

		// Gateway is accepted, check if listeners are ready
		if len(gateway.Status.Listeners) > 0 {
			allReady := true
			for _, listener := range gateway.Status.Listeners {
				// Check if listener is accepted
				listenerAccepted := false
				for _, listenerCondition := range listener.Conditions {
					if listenerCondition.Type == string(gatewayv1.ListenerConditionAccepted) {
						if listenerCondition.Status == metav1.ConditionTrue {
							listenerAccepted = true
							k.logger.Logf("[K8sClient] Gateway %s/%s listener %s is ACCEPTED", namespace, name, listener.Name)
						} else {
							k.logger.Logf("[K8sClient] Gateway %s/%s listener %s is NOT accepted: status=%s, reason=%s",
								namespace, name, listener.Name, listenerCondition.Status, listenerCondition.Reason)
						}
					}
				}
				if !listenerAccepted {
					allReady = false
					break
				}
			}
			if allReady {
				k.logger.Logf("[K8sClient] Gateway %s/%s is ready with all listeners accepted", namespace, name)
				return true, nil
			}
		} else {
			// Gateway is accepted but no listeners yet, continue waiting
			k.logger.Logf("[K8sClient] Gateway %s/%s is accepted but has no listener status yet, continuing to wait...", namespace, name)
			return false, nil
		}

		return false, nil
	})
}

// WaitForHTTPRouteReady waits for an HTTPRoute to be accepted
func (k *K8sClient) WaitForHTTPRouteReady(ctx context.Context, namespace, name string, timeout time.Duration) error {
	timeoutCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	return wait.PollUntilContextTimeout(timeoutCtx, TestPollInterval, timeout, true, func(ctx context.Context) (bool, error) {
		route, err := k.GetHTTPRoute(ctx, namespace, name)
		if err != nil {
			if errors.IsNotFound(err) {
				return false, nil
			}
			return false, err
		}

		// Log HTTPRoute status for debugging
		if len(route.Status.Parents) > 0 {
			for _, parentStatus := range route.Status.Parents {
				for _, condition := range parentStatus.Conditions {
					k.logger.Logf("[K8sClient] HTTPRoute %s/%s condition: %s=%s (reason: %s, message: %s)",
						namespace, name, condition.Type, condition.Status, condition.Reason, condition.Message)
				}
			}
		} else {
			k.logger.Logf("[K8sClient] HTTPRoute %s/%s has no parent status yet", namespace, name)
		}

		// Check if HTTPRoute is accepted
		for _, parentStatus := range route.Status.Parents {
			for _, condition := range parentStatus.Conditions {
				if condition.Type == string(gatewayv1.RouteConditionAccepted) && condition.Status == metav1.ConditionTrue {
					return true, nil
				}
			}
		}
		return false, nil
	})
}

// GetServiceByLabel gets a service by label selector
func (k *K8sClient) GetServiceByLabel(ctx context.Context, namespace, labelSelector string) (*corev1.Service, error) {
	selector, err := labels.Parse(labelSelector)
	if err != nil {
		return nil, fmt.Errorf("failed to parse label selector: %w", err)
	}

	services, err := k.clientset.CoreV1().Services(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: selector.String(),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list services: %w", err)
	}

	if len(services.Items) == 0 {
		return nil, fmt.Errorf("no services found with selector %s", labelSelector)
	}

	return &services.Items[0], nil
}

// RestartDeployment restarts a deployment
func (k *K8sClient) RestartDeployment(ctx context.Context, namespace, name string) error {
	deployment, err := k.clientset.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to get deployment: %w", err)
	}

	// Update annotation to trigger restart
	if deployment.Spec.Template.Annotations == nil {
		deployment.Spec.Template.Annotations = make(map[string]string)
	}
	deployment.Spec.Template.Annotations["kubectl.kubernetes.io/restartedAt"] = time.Now().Format(time.RFC3339)

	_, err = k.clientset.AppsV1().Deployments(namespace).Update(ctx, deployment, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("failed to update deployment: %w", err)
	}

	// Wait for rollout to complete
	return k.WaitForDeploymentReady(ctx, namespace, name, DeploymentTimeout)
}

// WaitForDeploymentReady waits for a deployment to be ready
func (k *K8sClient) WaitForDeploymentReady(ctx context.Context, namespace, name string, timeout time.Duration) error {
	k.logger.Logf("[K8sClient] Waiting for deployment %s/%s to be ready (timeout: %v)...", namespace, name, timeout)
	attempt := 0
	return wait.PollUntilContextTimeout(ctx, TestPollInterval, timeout, true, func(ctx context.Context) (bool, error) {
		// Check if context is cancelled
		if ctx.Err() != nil {
			return false, ctx.Err()
		}

		attempt++
		deployment, err := k.clientset.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			k.logger.Logf("[K8sClient] Error getting deployment %s/%s (attempt %d): %v", namespace, name, attempt, err)
			// Check if it's a context error - if so, return it to stop polling
			if ctx.Err() != nil {
				return false, ctx.Err()
			}
			// For other errors (like NotFound), return the error to stop polling
			return false, err
		}

		ready := deployment.Status.ReadyReplicas == *deployment.Spec.Replicas
		if !ready {
			if attempt%5 == 0 { // Log every 5 attempts
				k.logger.Logf("[K8sClient] Deployment %s/%s: %d/%d replicas ready (attempt %d)",
					namespace, name, deployment.Status.ReadyReplicas, *deployment.Spec.Replicas, attempt)
			}
		} else {
			k.logger.Logf("[K8sClient] Deployment %s/%s is ready: %d/%d replicas",
				namespace, name, deployment.Status.ReadyReplicas, *deployment.Spec.Replicas)
		}
		return ready, nil
	})
}

// CreateConfigMap creates or updates a ConfigMap
func (k *K8sClient) CreateConfigMap(ctx context.Context, namespace string, cm *corev1.ConfigMap) error {
	_, err := k.clientset.CoreV1().ConfigMaps(namespace).Create(ctx, cm, metav1.CreateOptions{})
	if err != nil {
		if errors.IsAlreadyExists(err) {
			// Update if exists
			_, err = k.clientset.CoreV1().ConfigMaps(namespace).Update(ctx, cm, metav1.UpdateOptions{})
			if err != nil {
				return fmt.Errorf("failed to update configmap: %w", err)
			}
			return nil
		}
		return fmt.Errorf("failed to create configmap: %w", err)
	}
	return nil
}

// GetConfigMap retrieves a ConfigMap
func (k *K8sClient) GetConfigMap(ctx context.Context, namespace, name string) (*corev1.ConfigMap, error) {
	cm, err := k.clientset.CoreV1().ConfigMaps(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to get configmap: %w", err)
	}
	return cm, nil
}

// ExecInPod executes a command in a pod container and returns stdout, stderr, and error
func (k *K8sClient) ExecInPod(ctx context.Context, namespace, podName, containerName string, command []string) (string, string, error) {
	if containerName == "" {
		return "", "", fmt.Errorf("container name cannot be empty")
	}

	// Use the RESTClient with proper parameter encoding
	req := k.clientset.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(podName).
		Namespace(namespace).
		SubResource("exec")

	// Set parameters using VersionedParams
	opts := &corev1.PodExecOptions{
		Container: containerName,
		Command:   command,
		Stdout:    true,
		Stderr:    true,
		TTY:       false,
	}

	req = req.VersionedParams(opts, metav1.ParameterCodec)

	exec, err := remotecommand.NewSPDYExecutor(k.config, "POST", req.URL())
	if err != nil {
		return "", "", fmt.Errorf("failed to create executor: %w", err)
	}

	var stdout, stderr bytes.Buffer
	err = exec.Stream(remotecommand.StreamOptions{
		Stdout: &stdout,
		Stderr: &stderr,
	})
	if err != nil {
		return stdout.String(), stderr.String(), fmt.Errorf("failed to execute command: %w", err)
	}

	return stdout.String(), stderr.String(), nil
}

// GetConfig returns the REST config
func (k *K8sClient) GetConfig() *rest.Config {
	return k.config
}

// GetClientset returns the clientset
func (k *K8sClient) GetClientset() *kubernetes.Clientset {
	return k.clientset
}

// GetDynamicClient returns the dynamic client
func (k *K8sClient) GetDynamicClient() dynamic.Interface {
	return k.dynamic
}
