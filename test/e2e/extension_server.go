package e2e

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/cli"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
)

// ExtensionServerDeployer handles deployment of the extension server
type ExtensionServerDeployer struct {
	k8sClient *K8sClient
	settings  *cli.EnvSettings
	cluster   *KindCluster
	logger    testLogger
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
		logger:    defaultLogger,
	}, nil
}

// SetLogger sets a custom logger for the deployer
func (e *ExtensionServerDeployer) SetLogger(logger testLogger) {
	e.logger = logger
}

// SetCluster sets the kind cluster for image loading
func (e *ExtensionServerDeployer) SetCluster(cluster *KindCluster) {
	e.cluster = cluster
}

// Deploy deploys the extension server using Helm
func (e *ExtensionServerDeployer) Deploy(ctx context.Context, namespace string) error {
	return e.DeployWithTLS(ctx, namespace, true, false, 0, "")
}

// DeployWithTLS deploys the extension server using Helm with TLS configuration
func (e *ExtensionServerDeployer) DeployWithTLS(ctx context.Context, namespace string, enablePlaintext bool, enableTLS bool, tlsPort int, tlsSecretName string) error {
	e.logger.Logf("[ExtensionServer] Deploying to %s...", namespace)
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

	// Image is assumed to be pre-built
	image := fmt.Sprintf("%s:%s", ExtensionServerImageRepo, ExtensionServerImageTag)

	// Load image into kind cluster (if cluster is available)
	if e.cluster != nil {
		if err := e.cluster.LoadImage(ctx, image); err != nil {
			return fmt.Errorf("failed to load image into kind: %w", err)
		}
	}

	// Install using Helm
	actionConfig := new(action.Configuration)
	// Use quiet Helm logger
	if err := actionConfig.Init(e.settings.RESTClientGetter(), namespace, "secret", func(format string, v ...interface{}) {
		msg := fmt.Sprintf(format, v...)
		if strings.Contains(msg, "Error") || strings.Contains(msg, "Failed") ||
			strings.Contains(msg, "Installing") || strings.Contains(msg, "installed") {
			e.logger.Logf("[ExtensionServer] Helm: %s", msg)
		}
	}); err != nil {
		return fmt.Errorf("failed to init helm action config: %w", err)
	}

	// Load Helm values from template
	valuesTemplateData := TemplateData{
		ExtensionServerImageRepo:       ExtensionServerImageRepo,
		ExtensionServerImageTag:        ExtensionServerImageTag,
		ImagePullPolicy:                 ImagePullPolicy,
		ExtensionServerEnablePlaintext:  enablePlaintext,
		ExtensionServerTLSEnabled:       enableTLS,
		ExtensionServerTLSPort:          tlsPort,
		ExtensionServerTLSSecretName:    tlsSecretName,
	}
	valuesTemplatePath := GetTemplatePath("extension-server-values.yaml")
	values, err := LoadHelmValues(valuesTemplatePath, valuesTemplateData)
	if err != nil {
		return fmt.Errorf("failed to load Helm values template: %w", err)
	}

	// Check if release exists
	histAction := action.NewHistory(actionConfig)
	histAction.Max = 1
	_, err = histAction.Run(ExtensionServerReleaseName)
	if err == nil {
		// Release exists, upgrade
		e.logger.Log("[ExtensionServer] Upgrading release...")
		upgradeAction := action.NewUpgrade(actionConfig)
		upgradeAction.Namespace = namespace
		upgradeAction.Wait = false
		upgradeAction.Timeout = HelmTimeout

		chart, err := loader.Load(chartPath)
		if err != nil {
			return fmt.Errorf("failed to load chart: %w", err)
		}

		_, err = upgradeAction.RunWithContext(ctx, ExtensionServerReleaseName, chart, values)
		if err != nil {
			return fmt.Errorf("failed to upgrade extension server: %w", err)
		}
		e.logger.Log("[ExtensionServer] Upgraded")
	} else {
		// Install new release
		e.logger.Log("[ExtensionServer] Installing...")
		installAction := action.NewInstall(actionConfig)
		installAction.ReleaseName = ExtensionServerReleaseName
		installAction.Namespace = namespace
		installAction.CreateNamespace = true
		installAction.Wait = false
		installAction.Timeout = HelmTimeout

		chart, err := loader.Load(chartPath)
		if err != nil {
			return fmt.Errorf("failed to load chart: %w", err)
		}

		_, err = installAction.RunWithContext(ctx, chart, values)
		if err != nil {
			return fmt.Errorf("failed to install extension server: %w", err)
		}
		e.logger.Log("[ExtensionServer] Installed")
	}

	// Wait for Helm chart to be applied (resources created)
	e.logger.Log("[ExtensionServer] Waiting for Helm chart to be applied...")
	if err := e.WaitForChartApplied(ctx, namespace); err != nil {
		return fmt.Errorf("failed to wait for Helm chart to be applied: %w", err)
	}
	e.logger.Log("[ExtensionServer] Helm chart applied successfully")

	return nil
}

// WaitForChartApplied waits for the Helm chart resources to be created
func (e *ExtensionServerDeployer) WaitForChartApplied(ctx context.Context, namespace string) error {
	// Wait for the deployment to exist (this indicates the chart was applied)
	// The deployment name follows the Helm fullname template: release-name-chart-name
	// Since release name is "xds-backend" and chart name is "xds-backend",
	// and release name contains chart name, the fullname is just "xds-backend"
	deploymentName := ExtensionServerReleaseName

	// Poll for deployment existence (not readiness - that's WaitForReady's job)
	timeoutCtx, cancel := context.WithTimeout(ctx, DeploymentTimeout)
	defer cancel()

	err := wait.PollUntilContextTimeout(timeoutCtx, TestPollInterval, DeploymentTimeout, true, func(ctx context.Context) (bool, error) {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}

		_, err := e.k8sClient.GetClientset().AppsV1().Deployments(namespace).Get(ctx, deploymentName, metav1.GetOptions{})
		if err != nil {
			if errors.IsNotFound(err) {
				// Deployment doesn't exist yet, keep polling
				return false, nil
			}
			// Other error, return it
			return false, err
		}

		// Deployment exists, chart was applied
		return true, nil
	})

	if err != nil {
		return fmt.Errorf("deployment %s/%s not found within timeout %v: %w", namespace, deploymentName, DeploymentTimeout, err)
	}

	return nil
}

// WaitForReady waits for the extension server to be ready
func (e *ExtensionServerDeployer) WaitForReady(ctx context.Context, namespace string) error {
	e.logger.Log("[ExtensionServer] Waiting for extension server to be ready...")
	err := e.k8sClient.WaitForPodsReady(ctx, namespace, ExtensionServerLabelSelector, TestTimeout)
	if err != nil {
		e.logger.Logf("[ExtensionServer] Extension server failed to become ready: %v", err)
	} else {
		e.logger.Log("[ExtensionServer] Extension server is ready!")
	}
	return err
}

// Uninstall uninstalls the extension server
func (e *ExtensionServerDeployer) Uninstall(ctx context.Context, namespace string) error {
	actionConfig := new(action.Configuration)
	if err := actionConfig.Init(e.settings.RESTClientGetter(), namespace, "secret", func(format string, v ...interface{}) {}); err != nil {
		return fmt.Errorf("failed to init helm action config: %w", err)
	}

	uninstallAction := action.NewUninstall(actionConfig)
	_, err := uninstallAction.Run(ExtensionServerReleaseName)
	if err != nil {
		return fmt.Errorf("failed to uninstall extension server: %w", err)
	}
	return nil
}
