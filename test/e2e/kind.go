package e2e

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/docker/docker/client"
	"gopkg.in/yaml.v2"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/kind/pkg/apis/config/v1alpha4"
	kindcluster "sigs.k8s.io/kind/pkg/cluster"
	"sigs.k8s.io/kind/pkg/cluster/nodeutils"
)

// KindCluster represents a kind cluster for testing
type KindCluster struct {
	name       string
	kubeconfig string
	provider   *kindcluster.Provider
	logger     testLogger
}

func NewKindCluster(name string) (*KindCluster, error) {
	if err := os.MkdirAll(KubeconfigDir, 0755); err != nil {
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

func (k *KindCluster) Create(ctx context.Context) error {
	k.logger.Logf("[KindCluster] Checking if cluster %s already exists...", k.name)
	existingClusters, err := k.provider.List()
	if err != nil {
		return fmt.Errorf("failed to list clusters: %w", err)
	}
	for _, clusterName := range existingClusters {
		if clusterName == k.name {
			k.logger.Logf("[KindCluster] Cluster %s already exists, skipping creation", k.name)
			return nil
		}
	}
	k.logger.Logf("[KindCluster] Cluster %s does not exist, creating new cluster...", k.name)

	configYAML, err := LoadTemplate("kind-cluster-config.yaml", KindClusterConfigTemplate{
		ClusterName:                    k.name,
		EnvoyGatewayContainerPort:      EnvoyGatewayContainerPort,
		EnvoyGatewayHostPort:           EnvoyGatewayHostPort,
		EnvoyGatewayHTTPSContainerPort: EnvoyGatewayHTTPSContainerPort,
		EnvoyGatewayHTTPSHostPort:      EnvoyGatewayHTTPSHostPort,
	})
	if err != nil {
		return fmt.Errorf("failed to load kind cluster config template: %w", err)
	}

	config := &v1alpha4.Cluster{}
	if err := yaml.Unmarshal([]byte(configYAML), config); err != nil {
		return fmt.Errorf("failed to unmarshal kind cluster config: %w", err)
	}

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

func (k *KindCluster) Delete(ctx context.Context) error {
	err := k.provider.Delete(k.name, k.kubeconfig)
	if err != nil {
		return fmt.Errorf("failed to delete kind cluster: %w", err)
	}
	os.Remove(k.kubeconfig)

	return nil
}

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

func (k *KindCluster) WaitForReady(ctx context.Context) error {
	k.logger.Logf("[KindCluster] Waiting for cluster %s...", k.name)

	timeoutCtx, cancel := context.WithTimeout(ctx, ClusterReadyTimeout)
	defer cancel()

	attempt := 0
	err := wait.PollUntilContextTimeout(timeoutCtx, TestPollInterval, ClusterReadyTimeout, true, func(ctx context.Context) (bool, error) {
		attempt++
		nodes, err := k.provider.ListNodes(k.name)
		if err != nil {
			return false, nil
		}
		if len(nodes) == 0 {
			return false, nil
		}
		return true, nil
	})

	if err != nil {
		return fmt.Errorf("timeout waiting for cluster to be ready after %d attempts: %w", attempt, err)
	}

	k.logger.Log("[KindCluster] Waiting for system pods to stabilize...")
	stabilizeCtx, stabilizeCancel := context.WithTimeout(timeoutCtx, StabilizeTimeout)
	defer stabilizeCancel()

	select {
	case <-stabilizeCtx.Done():
		if stabilizeCtx.Err() != context.DeadlineExceeded {
			return fmt.Errorf("context cancelled while waiting for system pods: %w", stabilizeCtx.Err())
		}
	case <-timeoutCtx.Done():
		return fmt.Errorf("parent context cancelled while waiting for system pods: %w", timeoutCtx.Err())
	}

	k.logger.Logf("[KindCluster] Cluster %s is ready", k.name)
	return nil
}

func (k *KindCluster) GetKubeconfigPath() string {
	return k.kubeconfig
}

func (k *KindCluster) LoadImage(ctx context.Context, image string) error {
	k.logger.Logf("[KindCluster] Loading Docker image %s into cluster %s...", image, k.name)

	nodeList, err := k.provider.ListInternalNodes(k.name)
	if err != nil {
		return fmt.Errorf("failed to list cluster nodes: %w", err)
	}
	if len(nodeList) == 0 {
		return fmt.Errorf("no nodes found for cluster %q", k.name)
	}

	dockerClient, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return fmt.Errorf("failed to create docker client: %w", err)
	}
	defer dockerClient.Close()

	for _, node := range nodeList {
		imageReader, err := dockerClient.ImageSave(ctx, []string{image})
		if err != nil {
			return fmt.Errorf("failed to save docker image for node %s: %w", node.String(), err)
		}

		if err := nodeutils.LoadImageArchive(node, imageReader); err != nil {
			imageReader.Close()
			return fmt.Errorf("failed to load image into node %s: %w", node.String(), err)
		}
		imageReader.Close()
	}

	k.logger.Logf("[KindCluster] Successfully loaded image %s into all nodes", image)
	return nil
}
