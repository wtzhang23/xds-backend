package e2e

import (
	"context"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextensionsclient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/tools/clientcmd"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayclient "sigs.k8s.io/gateway-api/pkg/client/clientset/versioned"
	k8syaml "sigs.k8s.io/yaml"
)

// K8sClient wraps Kubernetes client operations
type K8sClient struct {
	clientset     *kubernetes.Clientset
	dynamic       dynamic.Interface
	gatewayClient gatewayclient.Interface
	config        *rest.Config
	mapper        meta.RESTMapper
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

	// Create RESTMapper for automatic GVR conversion and scope detection
	discoveryClient := clientset.Discovery()
	cachedDiscoveryClient := memory.NewMemCacheClient(discoveryClient)
	mapper := restmapper.NewDeferredDiscoveryRESTMapper(cachedDiscoveryClient)

	return &K8sClient{
		clientset:     clientset,
		dynamic:       dynamicClient,
		gatewayClient: gatewayClient,
		config:        config,
		mapper:        mapper,
		logger:        defaultLogger,
	}, nil
}

// SetLogger sets a custom logger for the client
func (k *K8sClient) SetLogger(logger testLogger) {
	k.logger = logger
}

// CreateNamespace creates a namespace if it doesn't exist
func (k *K8sClient) CreateNamespace(ctx context.Context, name string) error {
	_, err := k.clientset.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
	}, metav1.CreateOptions{})
	if err != nil && !errors.IsAlreadyExists(err) {
		return fmt.Errorf("failed to create namespace: %w", err)
	}
	return nil
}

// WaitForPodsReady waits for pods matching the label selector to be ready
func (k *K8sClient) WaitForPodsReady(ctx context.Context, namespace, labelSelector string, timeout time.Duration) error {
	selector, err := labels.Parse(labelSelector)
	if err != nil {
		return fmt.Errorf("failed to parse label selector: %w", err)
	}

	return wait.PollUntilContextTimeout(ctx, TestPollInterval, timeout, true, func(ctx context.Context) (bool, error) {
		pods, err := k.clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
			LabelSelector: selector.String(),
		})
		if err != nil {
			return false, err
		}

		if len(pods.Items) == 0 {
			return false, nil
		}

		for _, pod := range pods.Items {
			if pod.Status.Phase != corev1.PodRunning {
				return false, nil
			}
			ready := false
			for _, condition := range pod.Status.Conditions {
				if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
					ready = true
					break
				}
			}
			if !ready {
				return false, nil
			}
		}
		return true, nil
	})
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

// ApplyYAML applies YAML resources using server-side apply
func (k *K8sClient) ApplyYAML(ctx context.Context, yamlContent string) error {
	yamlDocSeparator := regexp.MustCompile(`(?m)^---\s*$`)
	manifests := yamlDocSeparator.Split(yamlContent, -1)

	for _, manifest := range manifests {
		manifest = strings.TrimSpace(manifest)
		if manifest == "" {
			continue
		}

		manifestBytes := []byte(manifest)
		if len(manifestBytes) > 0 && manifestBytes[len(manifestBytes)-1] != '\n' {
			manifestBytes = append(manifestBytes, '\n')
		}

		var objMap map[string]interface{}
		if err := k8syaml.Unmarshal(manifestBytes, &objMap); err != nil {
			return fmt.Errorf("failed to unmarshal YAML: %w", err)
		}

		obj := &unstructured.Unstructured{Object: objMap}
		gvk := obj.GroupVersionKind()

		if err := k.applyUnstructured(ctx, obj, &gvk); err != nil {
			return fmt.Errorf("failed to apply resource %s/%s: %w", gvk.Kind, obj.GetName(), err)
		}
	}

	return nil
}

// applyUnstructured applies an unstructured object using server-side apply
func (k *K8sClient) applyUnstructured(ctx context.Context, obj *unstructured.Unstructured, gvk *schema.GroupVersionKind) error {
	_, err := k.getUnstructuredResource(ctx, obj, gvk)
	if errors.IsNotFound(err) {
		return k.createUnstructuredResource(ctx, obj, gvk)
	}
	return k.updateUnstructuredResource(ctx, obj, gvk)
}

// Helper methods for unstructured resources using dynamic client
func (k *K8sClient) getUnstructuredResource(ctx context.Context, obj *unstructured.Unstructured, gvk *schema.GroupVersionKind) (*unstructured.Unstructured, error) {
	resourceClient, err := k.getResourceClient(gvk, obj.GetNamespace())
	if err != nil {
		return nil, err
	}
	return resourceClient.Get(ctx, obj.GetName(), metav1.GetOptions{})
}

func (k *K8sClient) createUnstructuredResource(ctx context.Context, obj *unstructured.Unstructured, gvk *schema.GroupVersionKind) error {
	resourceClient, err := k.getResourceClient(gvk, obj.GetNamespace())
	if err != nil {
		return err
	}
	_, err = resourceClient.Create(ctx, obj, metav1.CreateOptions{})
	return err
}

func (k *K8sClient) updateUnstructuredResource(ctx context.Context, obj *unstructured.Unstructured, gvk *schema.GroupVersionKind) error {
	resourceClient, err := k.getResourceClient(gvk, obj.GetNamespace())
	if err != nil {
		return err
	}

	_, err = resourceClient.Apply(ctx, obj.GetName(), obj, metav1.ApplyOptions{
		FieldManager: "xds-backend-e2e-test",
	}, "")
	return err
}

// getResourceClient returns the appropriate resource client (namespaced or cluster-scoped)
// Uses RESTMapper to automatically determine GVR and scope
func (k *K8sClient) getResourceClient(gvk *schema.GroupVersionKind, namespace string) (dynamic.ResourceInterface, error) {
	// Use RESTMapper to get GVR from GVK (handles pluralization automatically)
	mapping, err := k.mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	if err != nil {
		return nil, fmt.Errorf("failed to get REST mapping for %s: %w", gvk, err)
	}

	baseClient := k.dynamic.Resource(mapping.Resource)

	// RESTMapper tells us if the resource is namespaced
	if mapping.Scope.Name() == meta.RESTScopeNameRoot {
		// Cluster-scoped resource
		return baseClient, nil
	}
	// Namespaced resource
	return baseClient.Namespace(namespace), nil
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

// GetHTTPRoute gets an HTTPRoute resource
func (k *K8sClient) GetHTTPRoute(ctx context.Context, namespace, name string) (*gatewayv1.HTTPRoute, error) {
	return k.gatewayClient.GatewayV1().HTTPRoutes(namespace).Get(ctx, name, metav1.GetOptions{})
}

// WaitForGatewayReady waits for a Gateway to be accepted and have listeners ready
func (k *K8sClient) WaitForGatewayReady(ctx context.Context, namespace, name string, timeout time.Duration) error {
	timeoutCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	return wait.PollUntilContextTimeout(timeoutCtx, TestPollInterval, timeout, true, func(ctx context.Context) (bool, error) {
		gateway, err := k.GetGateway(ctx, namespace, name)
		if err != nil {
			return false, err
		}

		gatewayAccepted := false
		for _, condition := range gateway.Status.Conditions {
			if condition.Type == string(gatewayv1.GatewayConditionAccepted) && condition.Status == metav1.ConditionTrue {
				gatewayAccepted = true
				break
			}
		}
		if !gatewayAccepted {
			return false, nil
		}

		if len(gateway.Status.Listeners) == 0 {
			return false, nil
		}

		for _, listener := range gateway.Status.Listeners {
			listenerAccepted := false
			for _, listenerCondition := range listener.Conditions {
				if listenerCondition.Type == string(gatewayv1.ListenerConditionAccepted) && listenerCondition.Status == metav1.ConditionTrue {
					listenerAccepted = true
					break
				}
			}
			if !listenerAccepted {
				return false, nil
			}
		}
		return true, nil
	})
}

// WaitForHTTPRouteReady waits for an HTTPRoute to be accepted
func (k *K8sClient) WaitForHTTPRouteReady(ctx context.Context, namespace, name string, timeout time.Duration) error {
	timeoutCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	return wait.PollUntilContextTimeout(timeoutCtx, TestPollInterval, timeout, true, func(ctx context.Context) (bool, error) {
		route, err := k.GetHTTPRoute(ctx, namespace, name)
		if err != nil {
			return false, err
		}

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

// DeleteSecret deletes a Secret resource
func (k *K8sClient) DeleteSecret(ctx context.Context, namespace, name string) error {
	return k.clientset.CoreV1().Secrets(namespace).Delete(ctx, name, metav1.DeleteOptions{})
}
