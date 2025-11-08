package e2e

import (
	"context"
	"fmt"
	"time"

	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/cli"
	"helm.sh/helm/v3/pkg/getter"
	"helm.sh/helm/v3/pkg/repo"
)

// EnvoyGatewayInstaller handles installation of Envoy Gateway
type EnvoyGatewayInstaller struct {
	k8sClient *K8sClient
	settings  *cli.EnvSettings
}

// NewEnvoyGatewayInstaller creates a new EnvoyGatewayInstaller
func NewEnvoyGatewayInstaller(kubeconfig string) (*EnvoyGatewayInstaller, error) {
	k8sClient, err := NewK8sClient(kubeconfig)
	if err != nil {
		return nil, err
	}

	settings := cli.New()
	settings.KubeConfig = kubeconfig

	return &EnvoyGatewayInstaller{
		k8sClient: k8sClient,
		settings:  settings,
	}, nil
}

// Install installs Envoy Gateway using Helm
func (e *EnvoyGatewayInstaller) Install(ctx context.Context) error {
	// Create namespace if it doesn't exist
	if err := e.k8sClient.CreateNamespace(ctx, EnvoyGatewayNamespace); err != nil {
		return fmt.Errorf("failed to create namespace: %w", err)
	}

	// Add Envoy Gateway Helm repo
	repoEntry := &repo.Entry{
		Name: "eg",
		URL:  "https://envoyproxy.github.io/gateway-helm",
	}

	chartRepo, err := repo.NewChartRepository(repoEntry, getter.All(e.settings))
	if err != nil {
		return fmt.Errorf("failed to create chart repository: %w", err)
	}

	_, err = chartRepo.DownloadIndexFile()
	if err != nil {
		// Try to add repo - ignore if already exists
		_ = err
	}

	// Update Helm repos
	repos := repo.NewFile()
	repos.Update(repoEntry)
	if err := repos.WriteFile(e.settings.RepositoryConfig, 0644); err != nil {
		return fmt.Errorf("failed to write repository config: %w", err)
	}

	// Get Helm action configuration
	actionConfig := new(action.Configuration)
	if err := actionConfig.Init(e.settings.RESTClientGetter(), EnvoyGatewayNamespace, "secret", func(format string, v ...interface{}) {
		// Log function - can be customized
	}); err != nil {
		return fmt.Errorf("failed to init helm action config: %w", err)
	}

	// Check if already installed
	installAction := action.NewInstall(actionConfig)
	installAction.ReleaseName = "eg"
	installAction.Namespace = EnvoyGatewayNamespace
	installAction.CreateNamespace = true
	installAction.Wait = false
	installAction.Timeout = 5 * time.Minute

	// Set values
	values := map[string]interface{}{
		"envoyGateway": map[string]interface{}{
			"extensionServer": map[string]interface{}{
				"enabled": true,
				"image": map[string]interface{}{
					"repository": "envoyproxy/gateway",
					"tag":        "latest",
				},
			},
		},
	}

	// Load chart
	chartPath, err := installAction.ChartPathOptions.LocateChart("eg/envoy-gateway", e.settings)
	if err != nil {
		return fmt.Errorf("failed to locate chart: %w", err)
	}

	chart, err := loader.Load(chartPath)
	if err != nil {
		return fmt.Errorf("failed to load chart: %w", err)
	}

	// Install
	_, err = installAction.RunWithContext(ctx, chart, values)
	if err != nil {
		// Check if already installed
		if contains(err.Error(), "already exists") {
			return nil
		}
		return fmt.Errorf("failed to install Envoy Gateway: %w", err)
	}

	return nil
}

// WaitForReady waits for Envoy Gateway to be ready
func (e *EnvoyGatewayInstaller) WaitForReady(ctx context.Context, namespace string) error {
	labelSelector := "app.kubernetes.io/name=envoy-gateway"
	return e.k8sClient.WaitForPodsReady(ctx, namespace, labelSelector, TestTimeout)
}

// Uninstall uninstalls Envoy Gateway
func (e *EnvoyGatewayInstaller) Uninstall(ctx context.Context) error {
	actionConfig := new(action.Configuration)
	if err := actionConfig.Init(e.settings.RESTClientGetter(), EnvoyGatewayNamespace, "secret", func(format string, v ...interface{}) {}); err != nil {
		return fmt.Errorf("failed to init helm action config: %w", err)
	}

	uninstallAction := action.NewUninstall(actionConfig)
	_, err := uninstallAction.Run("eg")
	if err != nil {
		return fmt.Errorf("failed to uninstall Envoy Gateway: %w", err)
	}
	return nil
}
