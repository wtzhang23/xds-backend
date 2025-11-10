package e2e

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v2"
	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/cli"
	"helm.sh/helm/v3/pkg/registry"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// EnvoyGatewayInstaller handles installation of Envoy Gateway
type EnvoyGatewayInstaller struct {
	k8sClient *K8sClient
	settings  *cli.EnvSettings
	chartDir  string
	logger    testLogger
}

// NewEnvoyGatewayInstaller creates a new EnvoyGatewayInstaller
func NewEnvoyGatewayInstaller(kubeconfig string) (*EnvoyGatewayInstaller, error) {
	k8sClient, err := NewK8sClient(kubeconfig)
	if err != nil {
		return nil, err
	}

	// Create temporary directory for charts
	chartDir := HelmChartDir
	if err := os.MkdirAll(chartDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create chart directory: %w", err)
	}

	settings := cli.New()
	settings.KubeConfig = kubeconfig
	// Use a temporary directory for Helm cache
	settings.RepositoryCache = HelmRepositoryDir
	settings.RepositoryConfig = HelmRepositoryConfig

	return &EnvoyGatewayInstaller{
		k8sClient: k8sClient,
		settings:  settings,
		chartDir:  chartDir,
		logger:    defaultLogger,
	}, nil
}

// SetLogger sets a custom logger for the installer
func (e *EnvoyGatewayInstaller) SetLogger(logger testLogger) {
	e.logger = logger
}

// Install installs Envoy Gateway using Helm
// extensionServerFQDN is the FQDN of the extension server (e.g., "xds-backend.xds-backend-system.svc.cluster.local:5005")
func (e *EnvoyGatewayInstaller) Install(ctx context.Context, extensionServerFQDN string, extensionServerPort int) error {
	e.logger.Log("[EnvoyGateway] Starting installation...")
	// Create namespace if it doesn't exist
	if err := e.k8sClient.CreateNamespace(ctx, EnvoyGatewayNamespace); err != nil {
		return fmt.Errorf("failed to create namespace: %w", err)
	}
	e.logger.Logf("[EnvoyGateway] Namespace %s ready", EnvoyGatewayNamespace)

	// Get Helm action configuration first
	e.logger.Log("[EnvoyGateway] Initializing Helm action configuration...")
	actionConfig := new(action.Configuration)
	// Use a quiet logger to reduce Helm verbosity - only log errors and important events
	if err := actionConfig.Init(e.settings.RESTClientGetter(), EnvoyGatewayNamespace, "secret", func(format string, v ...interface{}) {
		msg := fmt.Sprintf(format, v...)
		// Only log errors, failures, and installation completion
		if strings.Contains(msg, "Error") || strings.Contains(msg, "Failed") ||
			strings.Contains(msg, "Installing") || strings.Contains(msg, "installed") ||
			strings.Contains(msg, "Watching") {
			e.logger.Logf("[EnvoyGateway] Helm: %s", msg)
		}
	}); err != nil {
		return fmt.Errorf("failed to init helm action config: %w", err)
	}

	// Set up registry client for OCI charts
	registryClient, err := registry.NewClient(
		registry.ClientOptDebug(false),
		registry.ClientOptWriter(os.Stderr),
	)
	if err != nil {
		return fmt.Errorf("failed to create registry client: %w", err)
	}
	actionConfig.RegistryClient = registryClient

	// Envoy Gateway OCI chart path
	ociPath := EnvoyGatewayChartPath
	chartVersion := EnvoyGatewayChartVersion

	// Check if we have a cached chart and if it matches the expected version
	chartPath := e.findExistingChart()
	needsPull := true

	if chartPath != "" {
		existingVersion, err := e.getChartVersion(chartPath)
		if err == nil {
			// If using latest or versions match, reuse the cached chart
			if chartVersion == "" || existingVersion == chartVersion {
				e.logger.Logf("[EnvoyGateway] Reusing cached chart (version: %s)", existingVersion)
				needsPull = false
			} else {
				e.logger.Logf("[EnvoyGateway] Version mismatch (%s != %s), cleaning cache", existingVersion, chartVersion)
				// Clean up the mismatched chart
				if err := os.RemoveAll(e.chartDir); err != nil {
					e.logger.Logf("[EnvoyGateway] Warning: Failed to clean chart cache: %v", err)
				}
			}
		}
	}

	// Pull chart if needed
	if needsPull {
		e.logger.Logf("[EnvoyGateway] Pulling chart from %s...", ociPath)

		// Ensure chart directory exists
		if err := os.MkdirAll(e.chartDir, 0755); err != nil {
			return fmt.Errorf("failed to create chart directory: %w", err)
		}

		pullAction := action.NewPullWithOpts(action.WithConfig(actionConfig))
		pullAction.Settings = e.settings
		pullAction.Untar = true
		pullAction.UntarDir = e.chartDir
		pullAction.Version = chartVersion

		_, err := pullAction.Run(ociPath)
		if err != nil {
			return fmt.Errorf("failed to pull chart from %s: %w", ociPath, err)
		}

		// Find the extracted chart directory
		chartPath = e.findExistingChart()
		if chartPath == "" {
			return fmt.Errorf("failed to find extracted chart in %s", e.chartDir)
		}
		e.logger.Log("[EnvoyGateway] Chart pulled successfully")
	}

	// Load chart
	chart, err := loader.Load(chartPath)
	if err != nil {
		return fmt.Errorf("failed to load chart from %s: %w", chartPath, err)
	}

	return e.installChart(ctx, actionConfig, chart, chartPath, extensionServerFQDN, extensionServerPort)
}

// installChart performs the actual installation
func (e *EnvoyGatewayInstaller) installChart(ctx context.Context, actionConfig *action.Configuration, chart *chart.Chart, chartPath string, extensionServerFQDN string, extensionServerPort int) error {
	// Load Helm values from template
	valuesTemplateData := TemplateData{
		XdsBackendGroup:                XdsBackendGroup,
		XdsBackendAPIVersion:           XdsBackendAPIVersion,
		ExtensionServerFQDN:            extensionServerFQDN,
		ExtensionServerPort:            extensionServerPort,
		EnvoyGatewayContainerPort:      EnvoyGatewayContainerPort,
		EnvoyGatewayHTTPSContainerPort: EnvoyGatewayHTTPSContainerPort,
	}
	valuesTemplatePath := GetTemplatePath("envoy-gateway-values.yaml")
	values, err := LoadHelmValues(valuesTemplatePath, valuesTemplateData)
	if err != nil {
		return fmt.Errorf("failed to load Helm values template: %w", err)
	}

	// Debug: Log the rendered Helm values
	e.logger.Logf("[EnvoyGateway] Helm values configured for Envoy Gateway installation")

	// Check if already installed
	histAction := action.NewHistory(actionConfig)
	histAction.Max = 1
	_, err = histAction.Run(EnvoyGatewayReleaseName)
	if err == nil {
		e.logger.Log("[EnvoyGateway] Release already installed, upgrading...")
		// Upgrade instead of install
		upgradeAction := action.NewUpgrade(actionConfig)
		upgradeAction.Namespace = EnvoyGatewayNamespace
		upgradeAction.Wait = false
		upgradeAction.Timeout = HelmTimeout

		_, err = upgradeAction.RunWithContext(ctx, EnvoyGatewayReleaseName, chart, values)
		if err != nil {
			return fmt.Errorf("failed to upgrade Envoy Gateway: %w", err)
		}
		e.logger.Log("[EnvoyGateway] Helm release 'eg' upgraded successfully")
		return nil
	}
	e.logger.Log("[EnvoyGateway] Installing Envoy Gateway...")

	// Create install action
	installAction := action.NewInstall(actionConfig)
	installAction.ReleaseName = EnvoyGatewayReleaseName
	installAction.Namespace = EnvoyGatewayNamespace
	installAction.CreateNamespace = true
	installAction.Wait = false
	installAction.Timeout = HelmTimeout

	// Install
	e.logger.Log("[EnvoyGateway] Installing Helm release 'eg'...")
	_, err = installAction.RunWithContext(ctx, chart, values)
	if err != nil {
		// Check if already installed
		if strings.Contains(err.Error(), "already exists") {
			e.logger.Log("[EnvoyGateway] Release already exists, continuing...")
			return nil
		}
		return fmt.Errorf("failed to install Envoy Gateway: %w", err)
	}
	e.logger.Log("[EnvoyGateway] Helm release 'eg' installed successfully")

	return nil
}

// WaitForReady waits for Envoy Gateway to be ready
// Uses Kubernetes idiomatic wait.PollUntilContextTimeout for proper context handling
func (e *EnvoyGatewayInstaller) WaitForReady(ctx context.Context, namespace string) error {

	// Use a single timeout for the entire wait operation
	// This ensures the function will timeout rather than hang
	timeoutCtx, cancel := context.WithTimeout(ctx, TestTimeout)
	defer cancel()

	e.logger.Logf("[EnvoyGateway] Waiting for pods to be ready...")

	// Directly use WaitForPodsReady which already uses wait.PollUntilContextTimeout
	// This is the idiomatic Kubernetes way - no need to nest polling
	err := e.k8sClient.WaitForPodsReady(timeoutCtx, namespace, EnvoyGatewayLabelSelector, TestTimeout)
	if err != nil {
		// Extract final pod status for debugging
		e.printPodStatus(ctx, namespace, EnvoyGatewayLabelSelector)
		return fmt.Errorf("envoy gateway failed to become ready: %w", err)
	}

	e.logger.Log("[EnvoyGateway] Ready")
	return nil
}

// printPodStatus prints the status of pods for debugging
func (e *EnvoyGatewayInstaller) printPodStatus(ctx context.Context, namespace, labelSelector string) {
	pods, err := e.k8sClient.GetClientset().CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: labelSelector,
	})
	if err != nil {
		e.logger.Logf("Failed to list pods: %v", err)
		return
	}

	for _, pod := range pods.Items {
		e.logger.Logf("Pod %s/%s: Phase=%s, Ready=%v", namespace, pod.Name, pod.Status.Phase, isPodReady(pod))
		for _, condition := range pod.Status.Conditions {
			if condition.Status != corev1.ConditionTrue {
				e.logger.Logf("  Condition %s: %s - %s", condition.Type, condition.Status, condition.Message)
			}
		}
		if len(pod.Status.ContainerStatuses) > 0 {
			for _, cs := range pod.Status.ContainerStatuses {
				if !cs.Ready {
					e.logger.Logf("  Container %s: Ready=%v, State=%+v", cs.Name, cs.Ready, cs.State)
				}
			}
		}
	}
}

// isPodReady checks if a pod is ready
func isPodReady(pod corev1.Pod) bool {
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

// findExistingChart looks for an existing chart directory in the chart directory
func (e *EnvoyGatewayInstaller) findExistingChart() string {
	entries, err := os.ReadDir(e.chartDir)
	if err != nil {
		return ""
	}

	// Look for a directory that contains Chart.yaml
	for _, entry := range entries {
		if entry.IsDir() {
			candidatePath := filepath.Join(e.chartDir, entry.Name())
			chartYamlPath := filepath.Join(candidatePath, "Chart.yaml")
			if _, err := os.Stat(chartYamlPath); err == nil {
				return candidatePath
			}
		}
	}

	return ""
}

// getChartVersion reads the version from Chart.yaml
func (e *EnvoyGatewayInstaller) getChartVersion(chartPath string) (string, error) {
	chartYamlPath := filepath.Join(chartPath, "Chart.yaml")
	data, err := os.ReadFile(chartYamlPath)
	if err != nil {
		return "", fmt.Errorf("failed to read Chart.yaml: %w", err)
	}

	var chartMeta struct {
		Version string `yaml:"version"`
	}
	if err := yaml.Unmarshal(data, &chartMeta); err != nil {
		return "", fmt.Errorf("failed to parse Chart.yaml: %w", err)
	}

	return chartMeta.Version, nil
}

// Uninstall uninstalls Envoy Gateway
func (e *EnvoyGatewayInstaller) Uninstall(ctx context.Context) error {
	actionConfig := new(action.Configuration)
	if err := actionConfig.Init(e.settings.RESTClientGetter(), EnvoyGatewayNamespace, "secret", func(format string, v ...interface{}) {}); err != nil {
		return fmt.Errorf("failed to init helm action config: %w", err)
	}

	uninstallAction := action.NewUninstall(actionConfig)
	_, err := uninstallAction.Run(EnvoyGatewayReleaseName)
	if err != nil {
		return fmt.Errorf("failed to uninstall Envoy Gateway: %w", err)
	}
	return nil
}
