package e2e

import (
	"context"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// TestServiceDeployer handles deployment of a test HTTP service
type TestServiceDeployer struct {
	k8sClient *K8sClient
}

// NewTestServiceDeployer creates a new test service deployer
func NewTestServiceDeployer(kubeconfig string) (*TestServiceDeployer, error) {
	k8sClient, err := NewK8sClient(kubeconfig)
	if err != nil {
		return nil, err
	}
	return &TestServiceDeployer{
		k8sClient: k8sClient,
	}, nil
}

// Deploy deploys a test HTTP service
func (d *TestServiceDeployer) Deploy(ctx context.Context, namespace, name string, port int) error {
	// Create namespace if needed
	if err := d.k8sClient.CreateNamespace(ctx, namespace); err != nil {
		return fmt.Errorf("failed to create namespace: %w", err)
	}

	// Create deployment
	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: int32Ptr(1),
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app": name,
				},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app": name,
					},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "http-echo",
							Image: "hashicorp/http-echo:latest",
							Args: []string{
								"-text",
								"Hello from test service",
								"-listen",
								fmt.Sprintf(":%d", port),
							},
							Ports: []corev1.ContainerPort{
								{
									ContainerPort: int32(port),
									Name:          "http",
								},
							},
						},
					},
				},
			},
		},
	}

	_, err := d.k8sClient.GetClientset().AppsV1().Deployments(namespace).Create(ctx, deployment, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("failed to create deployment: %w", err)
	}

	// Create service
	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{
				"app": name,
			},
			Ports: []corev1.ServicePort{
				{
					Port:       int32(port),
					TargetPort: intstr.FromInt32(int32(port)),
					Protocol:   corev1.ProtocolTCP,
					Name:       "http",
				},
			},
		},
	}

	_, err = d.k8sClient.GetClientset().CoreV1().Services(namespace).Create(ctx, service, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("failed to create service: %w", err)
	}

	return nil
}

// WaitForReady waits for the test service to be ready
func (d *TestServiceDeployer) WaitForReady(ctx context.Context, namespace, name string) error {
	labelSelector := fmt.Sprintf("app=%s", name)
	return d.k8sClient.WaitForPodsReady(ctx, namespace, labelSelector, TestTimeout)
}

// GetServiceAddress returns the service address
func (d *TestServiceDeployer) GetServiceAddress(namespace, name string) string {
	return fmt.Sprintf("%s.%s.svc.cluster.local", name, namespace)
}

// GetServiceClusterIP gets the cluster IP of the service
func (d *TestServiceDeployer) GetServiceClusterIP(ctx context.Context, namespace, name string) (string, error) {
	return d.k8sClient.GetServiceClusterIP(ctx, namespace, name)
}

func int32Ptr(i int32) *int32 {
	return &i
}
