package e2e

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/cli"
)

// ExtensionServerDeployer handles deployment of the extension server
type ExtensionServerDeployer struct {
	k8sClient *K8sClient
	settings  *cli.EnvSettings
	cluster   *KindCluster
}

// NewExtensionServerDeployer creates a new ExtensionServerDeployer
func NewExtensionServerDeployer(kubeconfig string) (*ExtensionServerDeployer, error) {
	k8sClient, err := NewK8sClient(kubeconfig)
	if err != nil {
		return nil, err
	}

	settings := cli.New()
	settings.KubeConfig = kubeconfig

	return &ExtensionServerDeployer{
		k8sClient: k8sClient,
		settings:  settings,
	}, nil
}

// SetCluster sets the kind cluster for image loading
func (e *ExtensionServerDeployer) SetCluster(cluster *KindCluster) {
	e.cluster = cluster
}

// Deploy deploys the extension server using Helm
func (e *ExtensionServerDeployer) Deploy(ctx context.Context, namespace string) error {
	// Get the chart path - try to find project root
	chartPath := filepath.Join("..", "..", "charts", "xds-backend")
	if _, err := os.Stat(chartPath); os.IsNotExist(err) {
		// Try absolute path from current working directory
		cwd, err := os.Getwd()
		if err == nil {
			// If we're in test/e2e, go up two levels
			if filepath.Base(cwd) == "e2e" {
				chartPath = filepath.Join(filepath.Dir(filepath.Dir(cwd)), "charts", "xds-backend")
			} else {
				// Assume we're at project root
				chartPath = filepath.Join(cwd, "charts", "xds-backend")
			}
		}
		if _, err := os.Stat(chartPath); os.IsNotExist(err) {
			return fmt.Errorf("chart path does not exist: %s (tried relative and absolute paths)", chartPath)
		}
	}

	// Create namespace if it doesn't exist
	if err := e.k8sClient.CreateNamespace(ctx, namespace); err != nil {
		return fmt.Errorf("failed to create namespace: %w", err)
	}

	// Build and load the image into kind
	imageRepo := "wtzhang23/xds-backend-extension-server"
	imageTag := "e2e-test"
	image := fmt.Sprintf("%s:%s", imageRepo, imageTag)

	// Build the Docker image - find project root
	projectRoot := filepath.Dir(filepath.Dir(chartPath))
	dockerfilePath := filepath.Join(projectRoot, "Dockerfile")
	if _, err := os.Stat(dockerfilePath); os.IsNotExist(err) {
		return fmt.Errorf("dockerfile does not exist at: %s", dockerfilePath)
	}

	// Build Docker image (still using exec for now - could use docker client library)
	cmd := exec.CommandContext(ctx, "docker", "build", "-t", image, "-f", dockerfilePath, projectRoot)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to build Docker image: %w, output: %s", err, string(output))
	}

	// Load image into kind cluster
	if e.cluster != nil {
		if err := e.cluster.LoadImage(ctx, image); err != nil {
			return fmt.Errorf("failed to load image into kind: %w", err)
		}
	}

	// Install using Helm
	actionConfig := new(action.Configuration)
	if err := actionConfig.Init(e.settings.RESTClientGetter(), namespace, "secret", func(format string, v ...interface{}) {}); err != nil {
		return fmt.Errorf("failed to init helm action config: %w", err)
	}

	// Check if release exists
	histAction := action.NewHistory(actionConfig)
	histAction.Max = 1
	_, err = histAction.Run("xds-backend")
	if err == nil {
		// Release exists, upgrade
		upgradeAction := action.NewUpgrade(actionConfig)
		upgradeAction.Namespace = namespace
		upgradeAction.Wait = false
		upgradeAction.Timeout = 5 * time.Minute

		values := map[string]interface{}{
			"image": map[string]interface{}{
				"repository": imageRepo,
				"tag":        imageTag,
				"pullPolicy": "Never",
			},
		}

		chart, err := loader.Load(chartPath)
		if err != nil {
			return fmt.Errorf("failed to load chart: %w", err)
		}

		_, err = upgradeAction.RunWithContext(ctx, "xds-backend", chart, values)
		if err != nil {
			return fmt.Errorf("failed to upgrade extension server: %w", err)
		}
	} else {
		// Install new release
		installAction := action.NewInstall(actionConfig)
		installAction.ReleaseName = "xds-backend"
		installAction.Namespace = namespace
		installAction.CreateNamespace = true
		installAction.Wait = false
		installAction.Timeout = 5 * time.Minute

		values := map[string]interface{}{
			"image": map[string]interface{}{
				"repository": imageRepo,
				"tag":        imageTag,
				"pullPolicy": "Never",
			},
		}

		chart, err := loader.Load(chartPath)
		if err != nil {
			return fmt.Errorf("failed to load chart: %w", err)
		}

		_, err = installAction.RunWithContext(ctx, chart, values)
		if err != nil {
			return fmt.Errorf("failed to install extension server: %w", err)
		}
	}

	return nil
}

// WaitForReady waits for the extension server to be ready
func (e *ExtensionServerDeployer) WaitForReady(ctx context.Context, namespace string) error {
	labelSelector := "app.kubernetes.io/name=xds-backend"
	return e.k8sClient.WaitForPodsReady(ctx, namespace, labelSelector, TestTimeout)
}

// Uninstall uninstalls the extension server
func (e *ExtensionServerDeployer) Uninstall(ctx context.Context, namespace string) error {
	actionConfig := new(action.Configuration)
	if err := actionConfig.Init(e.settings.RESTClientGetter(), namespace, "secret", func(format string, v ...interface{}) {}); err != nil {
		return fmt.Errorf("failed to init helm action config: %w", err)
	}

	uninstallAction := action.NewUninstall(actionConfig)
	_, err := uninstallAction.Run("xds-backend")
	if err != nil {
		return fmt.Errorf("failed to uninstall extension server: %w", err)
	}
	return nil
}
