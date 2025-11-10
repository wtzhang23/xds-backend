package e2e

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/kind/pkg/apis/config/v1alpha4"
	kindcluster "sigs.k8s.io/kind/pkg/cluster"
)

// KindCluster represents a kind cluster for testing
type KindCluster struct {
	name       string
	kubeconfig string
	provider   *kindcluster.Provider
	logger     testLogger
}

// NewKindCluster creates a new KindCluster instance
func NewKindCluster(name string) (*KindCluster, error) {
	// Save kubeconfig in a gitignored directory for easier debugging
	// Path matches .gitignore pattern: test/e2e/.kubeconfig/
	if err := os.MkdirAll(KubeconfigDir, 0755); err != nil {
		// Fallback to temp dir if we can't create the directory
		return nil, fmt.Errorf("failed to create kubeconfig directory: %w", err)
	}
	kubeconfig := filepath.Join(KubeconfigDir, fmt.Sprintf("%s-kubeconfig", name))
	return &KindCluster{
		name:       name,
		kubeconfig: kubeconfig,
		provider:   kindcluster.NewProvider(),
		logger:     defaultLogger,
	}, nil
}

// SetLogger sets a custom logger for the cluster
func (k *KindCluster) SetLogger(logger testLogger) {
	k.logger = logger
}

// Create creates a new kind cluster
func (k *KindCluster) Create(ctx context.Context) error {
	k.logger.Logf("[KindCluster] Checking if cluster %s already exists...", k.name)
	// Check if cluster already exists
	existingClusters, err := k.provider.List()
	if err != nil {
		return fmt.Errorf("failed to list clusters: %w", err)
	}
	for _, clusterName := range existingClusters {
		if clusterName == k.name {
			k.logger.Logf("[KindCluster] Cluster %s already exists, skipping creation", k.name)
			return nil // Cluster already exists
		}
	}
	k.logger.Logf("[KindCluster] Cluster %s does not exist, creating new cluster...", k.name)

	// Create kind cluster config
	config := &v1alpha4.Cluster{
		Name: k.name,
		Nodes: []v1alpha4.Node{
			{
				Role: v1alpha4.ControlPlaneRole,
				KubeadmConfigPatches: []string{
					`kind: InitConfiguration
nodeRegistration:
  kubeletExtraArgs:
    node-labels: "ingress-ready=true"`,
				},
				ExtraPortMappings: []v1alpha4.PortMapping{
					{
						ContainerPort: EnvoyGatewayContainerPort,
						HostPort:      EnvoyGatewayHostPort,
						Protocol:      v1alpha4.PortMappingProtocolTCP,
					},
					{
						ContainerPort: EnvoyGatewayHTTPSContainerPort,
						HostPort:      EnvoyGatewayHTTPSHostPort,
						Protocol:      v1alpha4.PortMappingProtocolTCP,
					},
				},
			},
		},
	}

	// Create cluster
	k.logger.Logf("[KindCluster] Creating cluster with kubeconfig at: %s", k.kubeconfig)
	err = k.provider.Create(
		k.name,
		kindcluster.CreateWithV1Alpha4Config(config),
		kindcluster.CreateWithKubeconfigPath(k.kubeconfig),
	)
	if err != nil {
		return fmt.Errorf("failed to create kind cluster: %w", err)
	}
	k.logger.Logf("[KindCluster] Cluster %s created successfully", k.name)

	return nil
}

// Delete deletes the kind cluster
func (k *KindCluster) Delete(ctx context.Context) error {
	err := k.provider.Delete(k.name, k.kubeconfig)
	if err != nil {
		return fmt.Errorf("failed to delete kind cluster: %w", err)
	}

	// Clean up kubeconfig
	os.Remove(k.kubeconfig)

	return nil
}

// Exists checks if the cluster exists
func (k *KindCluster) Exists(ctx context.Context) bool {
	existingClusters, err := k.provider.List()
	if err != nil {
		return false
	}
	for _, clusterName := range existingClusters {
		if clusterName == k.name {
			return true
		}
	}
	return false
}

// WaitForReady waits for the cluster to be ready
func (k *KindCluster) WaitForReady(ctx context.Context) error {
	k.logger.Logf("[KindCluster] Waiting for cluster %s...", k.name)

	// Create a timeout context for the overall operation
	timeoutCtx, cancel := context.WithTimeout(ctx, ClusterReadyTimeout)
	defer cancel()

	attempt := 0
	err := wait.PollUntilContextTimeout(timeoutCtx, TestPollInterval, ClusterReadyTimeout, true, func(ctx context.Context) (bool, error) {
		attempt++
		nodes, err := k.provider.ListNodes(k.name)
		if err != nil {
			return false, nil // Continue polling on error
		}

		if len(nodes) == 0 {
			return false, nil // No nodes yet, continue polling
		}

		// If we can list nodes, assume they're ready
		return true, nil
	})

	if err != nil {
		return fmt.Errorf("timeout waiting for cluster to be ready after %d attempts: %w", attempt, err)
	}

	// Additional wait for system pods to stabilize
	k.logger.Log("[KindCluster] Waiting for system pods to stabilize...")
	stabilizeCtx, stabilizeCancel := context.WithTimeout(timeoutCtx, StabilizeTimeout)
	defer stabilizeCancel()

	// Use a simple select to wait for timeout or context cancellation
	select {
	case <-stabilizeCtx.Done():
		if stabilizeCtx.Err() != context.DeadlineExceeded {
			return fmt.Errorf("context cancelled while waiting for system pods: %w", stabilizeCtx.Err())
		}
		// Deadline exceeded is expected - we wanted to wait 10 seconds
	case <-timeoutCtx.Done():
		return fmt.Errorf("parent context cancelled while waiting for system pods: %w", timeoutCtx.Err())
	}

	k.logger.Logf("[KindCluster] Cluster %s is ready", k.name)
	return nil
}

// GetKubeconfigPath returns the path to the kubeconfig file
func (k *KindCluster) GetKubeconfigPath() string {
	return k.kubeconfig
}

// LoadImage loads a Docker image into the kind cluster
// For now, we use exec as this would require docker client library dependency
func (k *KindCluster) LoadImage(ctx context.Context, image string) error {
	k.logger.Logf("[KindCluster] Loading Docker image %s into cluster %s...", image, k.name)
	// TODO: Replace with docker client library + kind library utilities
	// The kind CLI uses docker save/load internally, so we'd need both libraries
	cmd := exec.CommandContext(ctx, "kind", "load", "docker-image", image, "--name", k.name)
	output, err := cmd.CombinedOutput()
	if err != nil {
		k.logger.Logf("[KindCluster] Failed to load image, output: %s", string(output))
		return fmt.Errorf("failed to load image: %w, output: %s", err, string(output))
	}
	k.logger.Logf("[KindCluster] Successfully loaded image %s", image)
	return nil
}

// WaitForPodReady waits for a pod to be ready in a namespace
// This is a placeholder - actual implementation should use k8s client
func (k *KindCluster) WaitForPodReady(ctx context.Context, namespace, labelSelector string, timeout time.Duration) error {
	// This will be handled by the Kubernetes client in other files
	timeoutCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Wait for the timeout or context cancellation
	<-timeoutCtx.Done()
	return fmt.Errorf("timeout waiting for pod to be ready: %w", timeoutCtx.Err())
}
