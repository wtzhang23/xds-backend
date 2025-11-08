package e2e

import (
	"context"
	"fmt"
	"net"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
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

	// Create an Endpoints resource pointing to the host
	endpoints := &corev1.Endpoints{
		ObjectMeta: metav1.ObjectMeta{
			Name:      e.serviceName,
			Namespace: e.namespace,
		},
		Subsets: []corev1.EndpointSubset{
			{
				Addresses: []corev1.EndpointAddress{
					{
						IP: hostIP,
					},
				},
				Ports: []corev1.EndpointPort{
					{
						Port:     int32(e.port),
						Protocol: corev1.ProtocolTCP,
						Name:     "grpc",
					},
				},
			},
		},
	}

	_, err = e.k8sClient.GetClientset().CoreV1().Endpoints(e.namespace).Create(ctx, endpoints, metav1.CreateOptions{})
	if err != nil {
		// Update if exists
		_, err = e.k8sClient.GetClientset().CoreV1().Endpoints(e.namespace).Update(ctx, endpoints, metav1.UpdateOptions{})
		if err != nil {
			return "", fmt.Errorf("failed to create/update endpoints: %w", err)
		}
	}

	// Create a headless service that uses the endpoints
	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      e.serviceName,
			Namespace: e.namespace,
		},
		Spec: corev1.ServiceSpec{
			Type:      corev1.ServiceTypeClusterIP,
			ClusterIP: "None", // Headless service
			Ports: []corev1.ServicePort{
				{
					Port:     int32(e.port),
					Protocol: corev1.ProtocolTCP,
					Name:     "grpc",
				},
			},
		},
	}

	_, err = e.k8sClient.GetClientset().CoreV1().Services(e.namespace).Create(ctx, service, metav1.CreateOptions{})
	if err != nil {
		// Update if exists
		_, err = e.k8sClient.GetClientset().CoreV1().Services(e.namespace).Update(ctx, service, metav1.UpdateOptions{})
		if err != nil {
			return "", fmt.Errorf("failed to create/update service: %w", err)
		}
	}

	// Return the service DNS name
	serviceAddress := fmt.Sprintf("%s.%s.svc.cluster.local", e.serviceName, e.namespace)
	return serviceAddress, nil
}

// getHostIP gets the IP address of the host that's accessible from the kind cluster
// We deploy a proxy pod with hostNetwork that can access localhost on the host
func (e *XdsServiceExposer) getHostIP(ctx context.Context) (string, error) {
	// Deploy a simple proxy pod with hostNetwork that will forward traffic
	// This pod runs with hostNetwork=true, so it can access localhost on the host
	proxyPodName := fmt.Sprintf("%s-proxy", e.serviceName)

	// Helper function for int32 pointer
	int32Ptr := func(i int32) *int32 { return &i }

	// Create a proxy deployment that uses hostNetwork
	proxyDeployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      proxyPodName,
			Namespace: e.namespace,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: int32Ptr(1),
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app": proxyPodName,
				},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app": proxyPodName,
					},
				},
				Spec: corev1.PodSpec{
					HostNetwork: true, // This allows access to host's localhost
					Containers: []corev1.Container{
						{
							Name:  "socat",
							Image: "alpine/socat:latest",
							Args: []string{
								"TCP-LISTEN:" + fmt.Sprintf("%d", e.port) + ",fork,reuseaddr",
								"TCP-CONNECT:127.0.0.1:" + fmt.Sprintf("%d", e.port),
							},
							Ports: []corev1.ContainerPort{
								{
									ContainerPort: int32(e.port),
									Protocol:      corev1.ProtocolTCP,
								},
							},
						},
					},
				},
			},
		},
	}

	_, err := e.k8sClient.GetClientset().AppsV1().Deployments(e.namespace).Create(ctx, proxyDeployment, metav1.CreateOptions{})
	if err != nil {
		// Update if exists
		_, err = e.k8sClient.GetClientset().AppsV1().Deployments(e.namespace).Update(ctx, proxyDeployment, metav1.UpdateOptions{})
		if err != nil {
			return "", fmt.Errorf("failed to create/update proxy deployment: %w", err)
		}
	}

	// Wait for the pod to be ready
	labelSelector := fmt.Sprintf("app=%s", proxyPodName)
	if err := e.k8sClient.WaitForPodsReady(ctx, e.namespace, labelSelector, 2*time.Minute); err != nil {
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

// Cleanup removes the service, endpoints, and proxy deployment
func (e *XdsServiceExposer) Cleanup(ctx context.Context) error {
	proxyPodName := fmt.Sprintf("%s-proxy", e.serviceName)
	_ = e.k8sClient.GetClientset().AppsV1().Deployments(e.namespace).Delete(ctx, proxyPodName, metav1.DeleteOptions{})
	_ = e.k8sClient.GetClientset().CoreV1().Services(e.namespace).Delete(ctx, e.serviceName, metav1.DeleteOptions{})
	_ = e.k8sClient.GetClientset().CoreV1().Endpoints(e.namespace).Delete(ctx, e.serviceName, metav1.DeleteOptions{})
	return nil
}
