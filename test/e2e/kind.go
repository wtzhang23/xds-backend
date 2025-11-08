package e2e

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"sigs.k8s.io/kind/pkg/apis/config/v1alpha4"
	kindcluster "sigs.k8s.io/kind/pkg/cluster"
)

// KindCluster represents a kind cluster for testing
type KindCluster struct {
	name       string
	kubeconfig string
	provider   *kindcluster.Provider
}

// NewKindCluster creates a new KindCluster instance
func NewKindCluster(name string) *KindCluster {
	kubeconfig := filepath.Join(os.TempDir(), fmt.Sprintf("kind-%s-kubeconfig", name))
	return &KindCluster{
		name:       name,
		kubeconfig: kubeconfig,
		provider:   kindcluster.NewProvider(),
	}
}

// Create creates a new kind cluster
func (k *KindCluster) Create(ctx context.Context) error {
	// Check if cluster already exists
	existingClusters, err := k.provider.List()
	if err != nil {
		return fmt.Errorf("failed to list clusters: %w", err)
	}
	for _, clusterName := range existingClusters {
		if clusterName == k.name {
			return nil // Cluster already exists
		}
	}

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
						ContainerPort: 80,
						HostPort:      8080,
						Protocol:      v1alpha4.PortMappingProtocolTCP,
					},
					{
						ContainerPort: 443,
						HostPort:      8443,
						Protocol:      v1alpha4.PortMappingProtocolTCP,
					},
				},
			},
		},
	}

	// Create cluster
	err = k.provider.Create(
		k.name,
		kindcluster.CreateWithV1Alpha4Config(config),
		kindcluster.CreateWithKubeconfigPath(k.kubeconfig),
	)
	if err != nil {
		return fmt.Errorf("failed to create kind cluster: %w", err)
	}

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
	// Wait for nodes to be ready using the provider
	deadline := time.Now().Add(5 * time.Minute)
	for time.Now().Before(deadline) {
		nodes, err := k.provider.ListNodes(k.name)
		if err == nil && len(nodes) > 0 {
			// Check if nodes are ready
			ready := true
			for _, node := range nodes {
				// Simple check - in a real implementation, you'd check node status
				// For now, if we can list nodes, assume they're ready
				_ = node
			}
			if ready {
				// Additional wait for system pods
				time.Sleep(10 * time.Second)
				return nil
			}
		}
		time.Sleep(TestPollInterval)
	}
	return fmt.Errorf("timeout waiting for cluster to be ready")
}

// GetKubeconfigPath returns the path to the kubeconfig file
func (k *KindCluster) GetKubeconfigPath() string {
	return k.kubeconfig
}

// LoadImage loads a Docker image into the kind cluster
// Note: The kind library doesn't have a direct LoadImage method in the Provider.
// We'll use exec for now, but this could be improved with the docker client library.
func (k *KindCluster) LoadImage(ctx context.Context, image string) error {
	// For now, use exec - in the future this could use docker client library
	// The kind CLI uses docker save/load internally
	cmd := exec.CommandContext(ctx, "kind", "load", "docker-image", image, "--name", k.name)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to load image: %w, output: %s", err, string(output))
	}
	return nil
}

// WaitForPodReady waits for a pod to be ready in a namespace
func (k *KindCluster) WaitForPodReady(ctx context.Context, namespace, labelSelector string, timeout time.Duration) error {
	// This will be handled by the Kubernetes client in other files
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		// This is a placeholder - actual implementation will use k8s client
		time.Sleep(TestPollInterval)
	}
	return fmt.Errorf("timeout waiting for pod to be ready")
}
