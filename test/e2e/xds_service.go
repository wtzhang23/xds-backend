package e2e

import (
	"context"
	"fmt"
	"net"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// XdsServiceExposer exposes the xDS server running on the host to the Kubernetes cluster
type XdsServiceExposer struct {
	k8sClient   *K8sClient
	namespace   string
	serviceName string
	port        int
}

// NewXdsServiceExposer creates a new xDS service exposer
func NewXdsServiceExposer(kubeconfig string, namespace, serviceName string, port int) (*XdsServiceExposer, error) {
	k8sClient, err := NewK8sClient(kubeconfig)
	if err != nil {
		return nil, err
	}
	return &XdsServiceExposer{
		k8sClient:   k8sClient,
		namespace:   namespace,
		serviceName: serviceName,
		port:        port,
	}, nil
}

// Expose creates a Kubernetes service that exposes the xDS server
// It uses a service with externalName pointing to the host, or creates an endpoint
// that points to the host IP accessible from the kind cluster
func (e *XdsServiceExposer) Expose(ctx context.Context, xdsServerAddress string) (string, error) {
	// Get the host IP that's accessible from the kind cluster
	// For kind, we can use the control plane node's IP or the host's network IP
	hostIP, err := e.getHostIP(ctx)
	if err != nil {
		return "", fmt.Errorf("failed to get host IP: %w", err)
	}

	// Create namespace if needed
	if err := e.k8sClient.CreateNamespace(ctx, e.namespace); err != nil {
		return "", fmt.Errorf("failed to create namespace: %w", err)
	}

	// Parse the xDS server address (we only need it for validation)
	_, _, err = net.SplitHostPort(xdsServerAddress)
	if err != nil {
		return "", fmt.Errorf("failed to parse xDS server address: %w", err)
	}

	// Load EndpointSlice template
	templateData := TemplateData{
		XdsServiceName: e.serviceName,
		TestNamespace:  e.namespace,
		XdsServerPort:  e.port,
		HostIP:         hostIP,
	}
	endpointSliceTemplatePath := GetTemplatePath("xds-endpointslice.yaml")
	endpointSliceYaml, err := LoadTemplate(endpointSliceTemplatePath, templateData)
	if err != nil {
		return "", fmt.Errorf("failed to load endpointslice template: %w", err)
	}
	if err := e.k8sClient.ApplyYAML(ctx, endpointSliceYaml); err != nil {
		return "", fmt.Errorf("failed to apply endpointslice: %w", err)
	}

	// Load service template
	serviceTemplatePath := GetTemplatePath("xds-service.yaml")
	serviceYaml, err := LoadTemplate(serviceTemplatePath, templateData)
	if err != nil {
		return "", fmt.Errorf("failed to load service template: %w", err)
	}
	if err := e.k8sClient.ApplyYAML(ctx, serviceYaml); err != nil {
		return "", fmt.Errorf("failed to apply service: %w", err)
	}

	// Return the service DNS name
	serviceAddress := fmt.Sprintf("%s.%s.svc.cluster.local", e.serviceName, e.namespace)
	return serviceAddress, nil
}

// getHostIP gets the IP address of the host that's accessible from the kind cluster
// We deploy a proxy pod with hostNetwork that can access localhost on the host
func (e *XdsServiceExposer) getHostIP(ctx context.Context) (string, error) {
	// Load proxy deployment template
	templateData := TemplateData{
		XdsServiceName: e.serviceName,
		TestNamespace:  e.namespace,
		XdsServerPort:  e.port,
	}
	proxyDeploymentTemplatePath := GetTemplatePath("xds-proxy-deployment.yaml")
	proxyDeploymentYaml, err := LoadTemplate(proxyDeploymentTemplatePath, templateData)
	if err != nil {
		return "", fmt.Errorf("failed to load proxy deployment template: %w", err)
	}
	if err := e.k8sClient.ApplyYAML(ctx, proxyDeploymentYaml); err != nil {
		return "", fmt.Errorf("failed to apply proxy deployment: %w", err)
	}

	// Wait for the pod to be ready
	proxyPodName := fmt.Sprintf("%s-proxy", e.serviceName)
	labelSelector := fmt.Sprintf("app=%s", proxyPodName)
	if err := e.k8sClient.WaitForPodsReady(ctx, e.namespace, labelSelector, DeploymentTimeout); err != nil {
		return "", fmt.Errorf("failed to wait for proxy pod: %w", err)
	}

	// Get the pod's IP (which will be the node's IP due to hostNetwork)
	pods, err := e.k8sClient.GetClientset().CoreV1().Pods(e.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: labelSelector,
	})
	if err != nil {
		return "", fmt.Errorf("failed to list proxy pods: %w", err)
	}

	if len(pods.Items) == 0 {
		return "", fmt.Errorf("no proxy pods found")
	}

	podIP := pods.Items[0].Status.PodIP
	if podIP == "" {
		return "", fmt.Errorf("proxy pod has no IP")
	}

	return podIP, nil
}

// Cleanup removes the service, endpointslice, and proxy deployment
func (e *XdsServiceExposer) Cleanup(ctx context.Context) error {
	proxyPodName := fmt.Sprintf("%s-proxy", e.serviceName)
	endpointSliceName := fmt.Sprintf("%s-1", e.serviceName)
	_ = e.k8sClient.GetClientset().AppsV1().Deployments(e.namespace).Delete(ctx, proxyPodName, metav1.DeleteOptions{})
	_ = e.k8sClient.GetClientset().CoreV1().Services(e.namespace).Delete(ctx, e.serviceName, metav1.DeleteOptions{})
	_ = e.k8sClient.GetClientset().DiscoveryV1().EndpointSlices(e.namespace).Delete(ctx, endpointSliceName, metav1.DeleteOptions{})
	return nil
}
