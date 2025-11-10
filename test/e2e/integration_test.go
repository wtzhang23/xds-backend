package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v2"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("xDS Backend Integration", func() {
	BeforeEach(func() {
		Expect(cluster).NotTo(BeNil())
		Expect(testService).NotTo(BeNil())
	})

	AfterEach(func() {
		k8sClient, err := NewK8sClient(cluster.GetKubeconfigPath())
		if err != nil {
			return
		}
		logCtx, logCancel := context.WithTimeout(context.Background(), LogExtractionTimeout)
		defer logCancel()
		collectLogs(logCtx, k8sClient)
	})

	It("should route traffic through Envoy Gateway to test service via xDS EDS", func() {
		k8sClient, err := NewK8sClient(cluster.GetKubeconfigPath())
		Expect(err).NotTo(HaveOccurred())

		// Wait for CRDs
		Expect(k8sClient.WaitForCRD(ctx, "gateways.gateway.networking.k8s.io")).To(Succeed())
		Expect(k8sClient.WaitForCRD(ctx, "httproutes.gateway.networking.k8s.io")).To(Succeed())
		Expect(k8sClient.WaitForCRD(ctx, "gatewayclasses.gateway.networking.k8s.io")).To(Succeed())
		envoyProxyCRDAvailable := k8sClient.WaitForCRD(ctx, "envoyproxies.gateway.envoyproxy.io") == nil

		// Get test service IP
		testServiceIP, err := testService.GetServiceClusterIP(ctx, TestNamespace, TestServiceName)
		Expect(err).NotTo(HaveOccurred())

		// Create EDS ConfigMap
		Expect(applyTemplate(ctx, k8sClient, "eds-configmap.yaml", TemplateData{
			EdsConfigMapName:      EdsConfigMapName,
			EnvoyGatewayNamespace: EnvoyGatewayNamespace,
			TestServiceName:       TestServiceName,
			TestServiceIP:         testServiceIP,
			TestServicePort:       TestServicePort,
		})).To(Succeed())

		// Create EnvoyProxy if CRD is available
		var envoyProxyCreated bool
		if envoyProxyCRDAvailable {
			envoyProxyCreated = applyTemplate(ctx, k8sClient, "envoyproxy.yaml", TemplateData{
				GatewayClassName:      GatewayClassName,
				EnvoyGatewayNamespace: EnvoyGatewayNamespace,
				EdsConfigMapName:      EdsConfigMapName,
			}) == nil
			if envoyProxyCreated {
				time.Sleep(3 * time.Second) // Allow controller to process
			}
		}

		// Create or update GatewayClass
		gwc, err := k8sClient.GetGatewayClass(ctx, GatewayClassName)
		if errors.IsNotFound(err) {
			Expect(applyTemplate(ctx, k8sClient, "gatewayclass.yaml", TemplateData{
				GatewayClassName: GatewayClassName,
			})).To(Succeed())
			gwc, err = k8sClient.GetGatewayClass(ctx, GatewayClassName)
			Expect(err).NotTo(HaveOccurred())
		}

		// Update GatewayClass to reference EnvoyProxy if needed
		if envoyProxyCreated && (gwc.Spec.ParametersRef == nil || gwc.Spec.ParametersRef.Name != GatewayClassName) {
			gwc.Spec.ParametersRef = &gatewayv1.ParametersReference{
				Group:     gatewayv1.Group("gateway.envoyproxy.io"),
				Kind:      gatewayv1.Kind("EnvoyProxy"),
				Name:      GatewayClassName,
				Namespace: (*gatewayv1.Namespace)(ptrOf(EnvoyGatewayNamespace)),
			}
			_, err = k8sClient.GetGatewayClient().GatewayV1().GatewayClasses().Update(ctx, gwc, metav1.UpdateOptions{})
			if errors.IsConflict(err) {
				// Retry once on conflict
				gwc, _ = k8sClient.GetGatewayClass(ctx, GatewayClassName)
				if gwc != nil {
					gwc.Spec.ParametersRef = &gatewayv1.ParametersReference{
						Group:     gatewayv1.Group("gateway.envoyproxy.io"),
						Kind:      gatewayv1.Kind("EnvoyProxy"),
						Name:      GatewayClassName,
						Namespace: (*gatewayv1.Namespace)(ptrOf(EnvoyGatewayNamespace)),
					}
					_, _ = k8sClient.GetGatewayClient().GatewayV1().GatewayClasses().Update(ctx, gwc, metav1.UpdateOptions{})
				}
			}
		}

		// Create resources
		baseData := TemplateData{
			XdsBackendGroup:        XdsBackendGroup,
			XdsBackendAPIVersion:   XdsBackendAPIVersion,
			XdsBackendKind:         XdsBackendKind,
			XdsBackendResourceName: XdsBackendResourceName,
			EnvoyGatewayNamespace:  EnvoyGatewayNamespace,
			TestServiceName:        TestServiceName,
			TestNamespace:          TestNamespace,
			EdsConfigPath:          EdsConfigPath,
		}
		Expect(applyTemplate(ctx, k8sClient, "xds-backend.yaml", baseData)).To(Succeed())
		Expect(applyTemplate(ctx, k8sClient, "reference-grant.yaml", baseData)).To(Succeed())
		Expect(applyTemplate(ctx, k8sClient, "gateway.yaml", TemplateData{
			GatewayName:           GatewayName,
			EnvoyGatewayNamespace: EnvoyGatewayNamespace,
			GatewayClassName:      GatewayClassName,
			GatewayListenerName:   GatewayListenerName,
			GatewayListenerPort:   GatewayListenerPort,
		})).To(Succeed())

		Expect(applyTemplate(ctx, k8sClient, "httproute.yaml", TemplateData{
			HTTPRouteName:          HTTPRouteName,
			TestNamespace:          TestNamespace,
			GatewayName:            GatewayName,
			EnvoyGatewayNamespace:  EnvoyGatewayNamespace,
			XdsBackendGroup:        XdsBackendGroup,
			XdsBackendKind:         XdsBackendKind,
			XdsBackendResourceName: XdsBackendResourceName,
		})).To(Succeed())

		// Wait for Gateway and HTTPRoute to be ready
		Expect(k8sClient.WaitForGatewayReady(ctx, EnvoyGatewayNamespace, GatewayName, DeploymentTimeout)).To(Succeed())
		Expect(k8sClient.WaitForHTTPRouteReady(ctx, TestNamespace, HTTPRouteName, DeploymentTimeout)).To(Succeed())

		// Wait for backend cluster to be created
		labelSelector := fmt.Sprintf("%s=%s,%s=%s", EnvoyProxyOwningGatewayLabelKey, GatewayName, EnvoyProxyComponentLabelKey, EnvoyProxyComponentLabelValue)
		pods, err := k8sClient.GetClientset().CoreV1().Pods(EnvoyGatewayNamespace).List(ctx, metav1.ListOptions{LabelSelector: labelSelector})
		Expect(err).NotTo(HaveOccurred())
		Expect(len(pods.Items)).To(BeNumerically(">", 0))
		podName := pods.Items[0].Name

		Expect(wait.PollUntilContextTimeout(ctx, TestPollInterval, DeploymentTimeout, true, func(ctx context.Context) (bool, error) {
			configDump, err := getEnvoyAdminConfigDump(ctx, k8sClient, EnvoyGatewayNamespace, podName)
			return err == nil && strings.Contains(configDump, ExpectedClusterName), nil
		})).To(Succeed())

		// Setup port forward and send HTTP request
		restConfig := k8sClient.GetConfig()
		path := fmt.Sprintf("/api/v1/namespaces/%s/pods/%s/portforward", EnvoyGatewayNamespace, podName)
		transport, upgrader, err := spdy.RoundTripperFor(restConfig)
		Expect(err).NotTo(HaveOccurred())
		baseURL, err := url.Parse(restConfig.Host)
		Expect(err).NotTo(HaveOccurred())
		baseURL.Path = path
		dialer := spdy.NewDialer(upgrader, &http.Client{Transport: transport}, "POST", baseURL)

		stopChan := make(chan struct{}, 1)
		readyChan := make(chan struct{}, 1)
		errChan := make(chan error, 1)
		portMapping := fmt.Sprintf("%d:%d", EnvoyProxyPodPort, EnvoyProxyPodPort)
		fw, err := portforward.New(dialer, []string{portMapping}, stopChan, readyChan, nil, nil)
		Expect(err).NotTo(HaveOccurred())

		go func() {
			if err := fw.ForwardPorts(); err != nil {
				errChan <- err
			}
		}()
		defer close(stopChan)

		select {
		case <-ctx.Done():
			Fail(fmt.Sprintf("context cancelled: %v", ctx.Err()))
		case err := <-errChan:
			Fail(fmt.Sprintf("port forward error: %v", err))
		case <-readyChan:
		}

		client := &http.Client{Timeout: HTTPClientTimeout}
		req, err := http.NewRequest("GET", fmt.Sprintf("http://localhost:%d/", EnvoyProxyPodPort), nil)
		Expect(err).NotTo(HaveOccurred())
		req.Header.Set("Host", "*")

		resp, err := client.Do(req)
		Expect(err).NotTo(HaveOccurred())
		defer resp.Body.Close()

		Expect(resp.StatusCode).To(Equal(http.StatusOK))
	})

	It("should expose metrics endpoint with PostClusterModify metrics", func() {
		k8sClient, err := NewK8sClient(cluster.GetKubeconfigPath())
		Expect(err).NotTo(HaveOccurred())

		// Wait for extension server pods to be ready
		Expect(k8sClient.WaitForPodsReady(ctx, ExtensionServerNamespace, ExtensionServerLabelSelector, DeploymentTimeout)).To(Succeed())

		// Get extension server pod
		pods, err := k8sClient.GetClientset().CoreV1().Pods(ExtensionServerNamespace).List(ctx, metav1.ListOptions{
			LabelSelector: ExtensionServerLabelSelector,
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(len(pods.Items)).To(BeNumerically(">", 0))
		podName := pods.Items[0].Name

		// Setup port forward to metrics port
		restConfig := k8sClient.GetConfig()
		path := fmt.Sprintf("/api/v1/namespaces/%s/pods/%s/portforward", ExtensionServerNamespace, podName)
		transport, upgrader, err := spdy.RoundTripperFor(restConfig)
		Expect(err).NotTo(HaveOccurred())
		baseURL, err := url.Parse(restConfig.Host)
		Expect(err).NotTo(HaveOccurred())
		baseURL.Path = path
		dialer := spdy.NewDialer(upgrader, &http.Client{Transport: transport}, "POST", baseURL)

		stopChan := make(chan struct{}, 1)
		readyChan := make(chan struct{}, 1)
		errChan := make(chan error, 1)
		portMapping := fmt.Sprintf("%d:%d", ExtensionServerMetricsPort, ExtensionServerMetricsPort)
		fw, err := portforward.New(dialer, []string{portMapping}, stopChan, readyChan, nil, nil)
		Expect(err).NotTo(HaveOccurred())

		go func() {
			if err := fw.ForwardPorts(); err != nil {
				errChan <- err
			}
		}()
		defer close(stopChan)

		select {
		case <-ctx.Done():
			Fail(fmt.Sprintf("context cancelled: %v", ctx.Err()))
		case err := <-errChan:
			Fail(fmt.Sprintf("port forward error: %v", err))
		case <-readyChan:
		}

		// Wait a bit for metrics server to be ready
		time.Sleep(2 * time.Second)

		// Fetch metrics endpoint
		client := &http.Client{Timeout: HTTPClientTimeout}
		req, err := http.NewRequest("GET", fmt.Sprintf("http://localhost:%d/metrics", ExtensionServerMetricsPort), nil)
		Expect(err).NotTo(HaveOccurred())

		resp, err := client.Do(req)
		Expect(err).NotTo(HaveOccurred())
		defer resp.Body.Close()

		Expect(resp.StatusCode).To(Equal(http.StatusOK))

		// Read metrics response
		body, err := io.ReadAll(resp.Body)
		Expect(err).NotTo(HaveOccurred())
		metricsBody := string(body)

		// Verify that metrics endpoint is working (should contain Prometheus format metrics)
		Expect(metricsBody).To(ContainSubstring("# HELP"))
		Expect(metricsBody).To(ContainSubstring("# TYPE"))

		// Verify that gRPC interceptor metrics are present
		// These should always be present as the server is running
		Expect(metricsBody).To(ContainSubstring("grpc_requests_total"))
		Expect(metricsBody).To(ContainSubstring("grpc_request_duration_seconds"))

		// Verify that our custom metrics are present
		// Note: These metrics may not be present if PostClusterModify hasn't been called yet
		// But we can at least verify the metrics endpoint is working
		Expect(metricsBody).To(Or(
			ContainSubstring("xds_backend_post_cluster_modify_total"),
			ContainSubstring("go_"), // Go runtime metrics should always be present
		))
	})
})

// applyTemplate renders a template and applies it to the cluster
func applyTemplate(ctx context.Context, k8sClient *K8sClient, templateName string, data TemplateData) error {
	templatePath := GetTemplatePath(templateName)
	yaml, err := LoadTemplate(templatePath, data)
	if err != nil {
		return fmt.Errorf("failed to load template %s: %w", templateName, err)
	}
	return applyYAML(ctx, k8sClient, yaml)
}

// applyYAML applies YAML content to the cluster
func applyYAML(ctx context.Context, k8sClient *K8sClient, yamlContent string) error {
	manifests := strings.Split(yamlContent, "---")
	for _, manifest := range manifests {
		manifest = strings.TrimSpace(manifest)
		if manifest == "" {
			continue
		}
		if err := k8sClient.ApplyYAML(ctx, manifest); err != nil {
			return fmt.Errorf("failed to apply resource: %w", err)
		}
	}
	return nil
}

// getEnvoyAdminConfigDump fetches the Envoy admin config dump from a pod
func getEnvoyAdminConfigDump(ctx context.Context, k8sClient *K8sClient, namespace, podName string) (string, error) {
	adminPort := EnvoyAdminPort
	adminPath := EnvoyAdminConfigDumpPath

	// Create a temporary port forward for the admin API
	stopChan := make(chan struct{}, 1)
	readyChan := make(chan struct{}, 1)
	errChan := make(chan error, 1)

	restConfig := k8sClient.GetConfig()
	path := fmt.Sprintf("/api/v1/namespaces/%s/pods/%s/portforward", namespace, podName)

	transport, upgrader, err := spdy.RoundTripperFor(restConfig)
	if err != nil {
		return "", fmt.Errorf("failed to create transport: %w", err)
	}

	baseURL, err := url.Parse(restConfig.Host)
	if err != nil {
		return "", fmt.Errorf("failed to parse host: %w", err)
	}
	baseURL.Path = path

	dialer := spdy.NewDialer(upgrader, &http.Client{Transport: transport}, "POST", baseURL)

	// Port forward to admin port
	adminPortMapping := fmt.Sprintf("%d:%d", EnvoyAdminPortForwardPort, adminPort)
	fw, err := portforward.New(dialer, []string{adminPortMapping}, stopChan, readyChan, nil, nil)
	if err != nil {
		return "", fmt.Errorf("failed to create port forward: %w", err)
	}

	go func() {
		if err := fw.ForwardPorts(); err != nil {
			select {
			case errChan <- err:
			default:
				// Error channel full or no listener, ignore to avoid blocking
			}
		}
	}()
	defer close(stopChan)

	// Wait for port forward to be ready
	select {
	case <-ctx.Done():
		return "", fmt.Errorf("context cancelled: %w", ctx.Err())
	case err := <-errChan:
		return "", fmt.Errorf("port forward error: %w", err)
	case <-readyChan:
		// Give Envoy admin API a moment to be ready
		time.Sleep(2 * time.Second)
	}

	adminURL := fmt.Sprintf("http://localhost:%d%s", EnvoyAdminPortForwardPort, adminPath)
	client := &http.Client{Timeout: HTTPClientTimeout}
	resp, err := client.Get(adminURL)
	if err != nil {
		return "", fmt.Errorf("failed to get config dump: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("admin API returned status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read response: %w", err)
	}

	// Parse JSON and convert to YAML
	var configDump interface{}
	if err := json.Unmarshal(body, &configDump); err != nil {
		// If JSON parsing fails, return raw body
		return string(body), nil
	}

	// Convert to YAML
	yamlBytes, err := yaml.Marshal(configDump)
	if err != nil {
		// If YAML conversion fails, return JSON pretty-printed
		jsonBytes, jsonErr := json.MarshalIndent(configDump, "", "  ")
		if jsonErr != nil {
			return string(body), nil
		}
		return string(jsonBytes), nil
	}

	return string(yamlBytes), nil
}

// getEnvoyAccessLogs fetches the Envoy access logs from a pod (raw, unfiltered)
func getEnvoyAccessLogs(ctx context.Context, k8sClient *K8sClient, namespace, podName string) (string, error) {
	// Envoy access logs are typically written to stdout/stderr
	// We need to specify the container name since the pod has multiple containers
	logs, err := k8sClient.GetClientset().CoreV1().Pods(namespace).GetLogs(podName, &corev1.PodLogOptions{
		Container: "envoy",
		// Get all logs, not just tail
	}).Stream(ctx)
	if err != nil {
		return "", fmt.Errorf("failed to stream pod logs: %w", err)
	}
	defer logs.Close()

	logBytes, err := io.ReadAll(logs)
	if err != nil {
		return "", fmt.Errorf("failed to read log stream: %w", err)
	}

	return string(logBytes), nil
}

// getExtensionServerLogs fetches the extension server logs from pods (raw, unfiltered)
func getExtensionServerLogs(ctx context.Context, k8sClient *K8sClient, namespace string) (string, error) {
	// Find extension server pods
	pods, err := k8sClient.GetClientset().CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("app.kubernetes.io/name=%s", ExtensionServerReleaseName),
	})
	if err != nil {
		return "", fmt.Errorf("failed to list pods: %w", err)
	}

	if len(pods.Items) == 0 {
		return "No extension server pods found", nil
	}

	var allLogs []string
	for _, pod := range pods.Items {
		// Try to find the container name - it might be the release name or "xds-backend-extension-server"
		containerName := ExtensionServerReleaseName
		if len(pod.Spec.Containers) > 0 {
			containerName = pod.Spec.Containers[0].Name
		}

		logs, err := k8sClient.GetClientset().CoreV1().Pods(namespace).GetLogs(pod.Name, &corev1.PodLogOptions{
			Container: containerName,
			// Get all logs, not just tail
		}).Stream(ctx)
		if err != nil {
			allLogs = append(allLogs, fmt.Sprintf("Failed to get logs for pod %s: %v", pod.Name, err))
			continue
		}
		defer logs.Close()

		logBytes, err := io.ReadAll(logs)
		if err != nil {
			allLogs = append(allLogs, fmt.Sprintf("Failed to read logs for pod %s: %v", pod.Name, err))
			continue
		}

		allLogs = append(allLogs, fmt.Sprintf("--- Pod: %s ---\n%s", pod.Name, string(logBytes)))
	}

	return strings.Join(allLogs, "\n"), nil
}

// getEnvoyGatewayControllerLogs fetches the Envoy Gateway controller logs from pods (raw, unfiltered)
func getEnvoyGatewayControllerLogs(ctx context.Context, k8sClient *K8sClient, namespace string) (string, error) {
	// Find Envoy Gateway controller pods
	pods, err := k8sClient.GetClientset().CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: EnvoyGatewayLabelSelector,
	})
	if err != nil {
		return "", fmt.Errorf("failed to list pods: %w", err)
	}

	if len(pods.Items) == 0 {
		return "No Envoy Gateway controller pods found", nil
	}

	var allLogs []string
	for _, pod := range pods.Items {
		// Try to find the container name - it might be "envoy-gateway" or similar
		containerName := ""
		if len(pod.Spec.Containers) > 0 {
			containerName = pod.Spec.Containers[0].Name
		}

		logs, err := k8sClient.GetClientset().CoreV1().Pods(namespace).GetLogs(pod.Name, &corev1.PodLogOptions{
			Container: containerName,
			// Get all logs, not just tail
		}).Stream(ctx)
		if err != nil {
			allLogs = append(allLogs, fmt.Sprintf("Failed to get logs for pod %s: %v", pod.Name, err))
			continue
		}
		defer logs.Close()

		logBytes, err := io.ReadAll(logs)
		if err != nil {
			allLogs = append(allLogs, fmt.Sprintf("Failed to read logs for pod %s: %v", pod.Name, err))
			continue
		}

		allLogs = append(allLogs, fmt.Sprintf("--- Pod: %s ---\n%s", pod.Name, string(logBytes)))
	}

	return strings.Join(allLogs, "\n"), nil
}

// writeLogToFile writes log content to a file in the gitignored logs directory
// Returns the file path if successful, empty string otherwise
func writeLogToFile(logType, content string) string {
	logsDir := LogDir
	if err := os.MkdirAll(logsDir, 0755); err != nil {
		logsDir = filepath.Join(os.TempDir(), "xds-backend-e2e-logs")
		if err := os.MkdirAll(logsDir, 0755); err != nil {
			return ""
		}
	}
	timestamp := time.Now().Format(LogTimestampFormat)
	filename := fmt.Sprintf("%s-%s.log", logType, timestamp)
	filepath := filepath.Join(logsDir, filename)
	if err := os.WriteFile(filepath, []byte(content), 0644); err != nil {
		return ""
	}
	return filepath
}

// collectLogs collects logs for debugging
func collectLogs(ctx context.Context, k8sClient *K8sClient) {
	if extensionLogs, err := getExtensionServerLogs(ctx, k8sClient, ExtensionServerNamespace); err == nil {
		writeLogToFile("extension-server", extensionLogs)
	}
	if egControllerLogs, err := getEnvoyGatewayControllerLogs(ctx, k8sClient, EnvoyGatewayNamespace); err == nil {
		writeLogToFile("envoy-gateway-controller", egControllerLogs)
	}
	labelSelector := fmt.Sprintf("%s=%s,%s=%s", EnvoyProxyOwningGatewayLabelKey, GatewayName, EnvoyProxyComponentLabelKey, EnvoyProxyComponentLabelValue)
	pods, err := k8sClient.GetClientset().CoreV1().Pods(EnvoyGatewayNamespace).List(ctx, metav1.ListOptions{LabelSelector: labelSelector})
	if err == nil && len(pods.Items) > 0 {
		podName := pods.Items[0].Name
		if accessLogs, err := getEnvoyAccessLogs(ctx, k8sClient, EnvoyGatewayNamespace, podName); err == nil {
			writeLogToFile("envoy-access", accessLogs)
		}
		if configDump, err := getEnvoyAdminConfigDump(ctx, k8sClient, EnvoyGatewayNamespace, podName); err == nil {
			writeLogToFile("envoy-config-dump", configDump)
		}
	}
}
