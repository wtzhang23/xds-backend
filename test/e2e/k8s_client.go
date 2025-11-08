package e2e

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// K8sClient wraps Kubernetes client operations
type K8sClient struct {
	clientset *kubernetes.Clientset
	config    *rest.Config
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

	return &K8sClient{
		clientset: clientset,
		config:    config,
	}, nil
}

// CreateNamespace creates a namespace if it doesn't exist
func (k *K8sClient) CreateNamespace(ctx context.Context, name string) error {
	_, err := k.clientset.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
	}, metav1.CreateOptions{})
	if err != nil {
		// Ignore if namespace already exists
		if !isAlreadyExists(err) {
			return fmt.Errorf("failed to create namespace: %w", err)
		}
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

		// Check if all pods are ready
		for _, pod := range pods.Items {
			if pod.Status.Phase != corev1.PodRunning {
				return false, nil
			}
			for _, condition := range pod.Status.Conditions {
				if condition.Type == corev1.PodReady && condition.Status != corev1.ConditionTrue {
					return false, nil
				}
			}
		}

		return true, nil
	})
}

// GetServiceClusterIP gets the cluster IP of a service
func (k *K8sClient) GetServiceClusterIP(ctx context.Context, namespace, name string) (string, error) {
	svc, err := k.clientset.CoreV1().Services(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("failed to get service: %w", err)
	}
	return svc.Spec.ClusterIP, nil
}

// ApplyManifest applies a Kubernetes manifest
func (k *K8sClient) ApplyManifest(ctx context.Context, manifest string) error {
	// Use kubectl apply via exec for now, or implement client-side apply
	// For a more complete solution, you'd use client-side apply or server-side apply
	return fmt.Errorf("not implemented - use kubectl apply or implement client-side apply")
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
	return k.WaitForDeploymentReady(ctx, namespace, name, 2*time.Minute)
}

// WaitForDeploymentReady waits for a deployment to be ready
func (k *K8sClient) WaitForDeploymentReady(ctx context.Context, namespace, name string, timeout time.Duration) error {
	return wait.PollUntilContextTimeout(ctx, TestPollInterval, timeout, true, func(ctx context.Context) (bool, error) {
		deployment, err := k.clientset.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return false, err
		}

		return deployment.Status.ReadyReplicas == *deployment.Spec.Replicas, nil
	})
}

// CreateConfigMap creates or updates a ConfigMap
func (k *K8sClient) CreateConfigMap(ctx context.Context, namespace string, cm *corev1.ConfigMap) error {
	_, err := k.clientset.CoreV1().ConfigMaps(namespace).Create(ctx, cm, metav1.CreateOptions{})
	if err != nil {
		if isAlreadyExists(err) {
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

// GetConfig returns the REST config
func (k *K8sClient) GetConfig() *rest.Config {
	return k.config
}

// GetClientset returns the clientset
func (k *K8sClient) GetClientset() *kubernetes.Clientset {
	return k.clientset
}

// Helper function to check if error is "already exists"
func isAlreadyExists(err error) bool {
	if err == nil {
		return false
	}
	// Check for common "already exists" error patterns
	return err.Error() != "" && (contains(err.Error(), "already exists") ||
		contains(err.Error(), "AlreadyExists"))
}

// Helper to create objects from YAML (simplified - in production use proper YAML parsing)
// This is a placeholder function - not currently used
// func createFromYAML(scheme *runtime.Scheme, yaml string) (runtime.Object, error) {
// 	// This is a placeholder - in a real implementation, you'd use
// 	// sigs.k8s.io/yaml or similar to parse YAML and decode into objects
// 	return nil, fmt.Errorf("not implemented")
// }
