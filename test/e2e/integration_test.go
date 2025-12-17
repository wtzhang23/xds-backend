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
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	grpc_health_v1 "google.golang.org/grpc/health/grpc_health_v1"
	"gopkg.in/yaml.v2"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func extractClusterNames(configDump string) []string {
	clusterNamePattern := regexp.MustCompile(`"name"\s*:\s*"([^"]+)"`)
	matches := clusterNamePattern.FindAllStringSubmatch(configDump, -1)

	clusters := make(map[string]bool)
	for _, match := range matches {
		if len(match) >= 2 {
			clusters[match[1]] = true
		}
	}

	result := make([]string, 0, len(clusters))
	for name := range clusters {
		result = append(result, name)
	}
	return result
}

func parseMetricValue(metricsBody, metricName string) float64 {
	pattern := regexp.MustCompile(fmt.Sprintf(`%s\{[^}]*\}\s+([0-9.]+)`, regexp.QuoteMeta(metricName)))
	matches := pattern.FindAllStringSubmatch(metricsBody, -1)

	var total float64
	for _, match := range matches {
		if len(match) >= 2 {
			value, err := strconv.ParseFloat(match[1], 64)
			if err == nil {
				total += value
			}
		}
	}
	return total
}

var _ = Describe("xDS Backend Integration", func() {
	var testStartTime time.Time

	BeforeEach(func() {
		Expect(cluster).NotTo(BeNil())
		Expect(testService).NotTo(BeNil())
		testStartTime = time.Now()
	})

	AfterEach(func() {
		k8sClient, err := NewK8sClient(cluster.GetKubeconfigPath())
		if err != nil {
			return
		}
		logCtx, logCancel := context.WithTimeout(context.Background(), LogExtractionTimeout)
		defer logCancel()
		collectLogs(logCtx, k8sClient, testStartTime)
	})

	It("should route traffic through Envoy Gateway to test service via file-based EDS", func() {
		k8sClient, err := NewK8sClient(cluster.GetKubeconfigPath())
		Expect(err).NotTo(HaveOccurred())

		Expect(applyTemplate(ctx, k8sClient, "xds-backend.yaml", XdsBackendTemplate{
			XdsBackendGroup:        XdsBackendGroup,
			XdsBackendAPIVersion:   XdsBackendAPIVersion,
			XdsBackendKind:         XdsBackendKind,
			XdsBackendResourceName: XdsBackendResourceName,
			EnvoyGatewayNamespace:  EnvoyGatewayNamespace,
			EdsConfigPath:          FileXdsEdsConfigPath,
			TestServiceName:        TestServiceName,
		})).To(Succeed())

		Expect(applyTemplate(ctx, k8sClient, "httproute.yaml", HTTPRouteTemplate{
			HTTPRouteName:          HTTPRouteName,
			GatewayName:            GatewayName,
			EnvoyGatewayNamespace:  EnvoyGatewayNamespace,
			XdsBackendGroup:        XdsBackendGroup,
			XdsBackendKind:         XdsBackendKind,
			XdsBackendResourceName: XdsBackendResourceName,
			HTTPRoutePathPrefix:    HTTPRoutePathPrefixFile,
		})).To(Succeed())

		Expect(k8sClient.WaitForHTTPRouteReady(ctx, EnvoyGatewayNamespace, HTTPRouteName, DeploymentTimeout)).To(Succeed())

		labelSelector := fmt.Sprintf("%s=%s,%s=%s", EnvoyProxyOwningGatewayLabelKey, GatewayName, EnvoyProxyComponentLabelKey, EnvoyProxyComponentLabelValue)
		pods, err := k8sClient.GetClientset().CoreV1().Pods(EnvoyGatewayNamespace).List(ctx, metav1.ListOptions{LabelSelector: labelSelector})
		Expect(err).NotTo(HaveOccurred())
		Expect(len(pods.Items)).To(BeNumerically(">", 0))
		podName := pods.Items[0].Name

		Expect(wait.PollUntilContextTimeout(ctx, TestPollInterval, DeploymentTimeout, true, func(ctx context.Context) (bool, error) {
			configDump, err := getEnvoyAdminConfigDump(ctx, k8sClient, EnvoyGatewayNamespace, podName)
			return err == nil && strings.Contains(configDump, ExpectedClusterName), nil
		})).To(Succeed())
		restConfig := k8sClient.GetConfig()
		portMapping := fmt.Sprintf("%d:%d", EnvoyProxyPodPort, EnvoyProxyPodPort)
		pf, err := SetupPortForward(ctx, restConfig, EnvoyGatewayNamespace, podName, []string{portMapping})
		Expect(err).NotTo(HaveOccurred())
		defer pf.Stop()

		var resp *http.Response
		Expect(wait.PollUntilContextTimeout(ctx, TestPollInterval, HTTPRequestTimeout, true, func(ctx context.Context) (bool, error) {
			client := &http.Client{Timeout: HTTPClientTimeout}
			req, err := http.NewRequestWithContext(ctx, "GET", fmt.Sprintf("http://localhost:%d%s", EnvoyProxyPodPort, HTTPRoutePathPrefixFile), nil)
			if err != nil {
				return false, err
			}
			req.Header.Set("Host", "*")

			attemptResp, err := client.Do(req)
			if err != nil {
				return false, nil
			}

			if attemptResp.StatusCode == http.StatusOK {
				resp = attemptResp
				return true, nil
			}
			attemptResp.Body.Close()
			return false, nil
		})).To(Succeed())
		defer resp.Body.Close()
		Expect(resp.StatusCode).To(Equal(http.StatusOK))

		time.Sleep(EnvoyAccessLogFlushDelay)
	})

	It("should expose metrics endpoint with PostClusterModify metrics", func() {
		k8sClient, err := NewK8sClient(cluster.GetKubeconfigPath())
		Expect(err).NotTo(HaveOccurred())

		Expect(k8sClient.WaitForPodsReady(ctx, ExtensionServerNamespace, ExtensionServerLabelSelector, DeploymentTimeout)).To(Succeed())
		pods, err := k8sClient.GetClientset().CoreV1().Pods(ExtensionServerNamespace).List(ctx, metav1.ListOptions{
			LabelSelector: ExtensionServerLabelSelector,
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(len(pods.Items)).To(BeNumerically(">", 0))
		podName := pods.Items[0].Name

		restConfig := k8sClient.GetConfig()
		portMapping := fmt.Sprintf("%d:%d", ExtensionServerMetricsPort, ExtensionServerMetricsPort)
		pf, err := SetupPortForward(ctx, restConfig, ExtensionServerNamespace, podName, []string{portMapping})
		Expect(err).NotTo(HaveOccurred())
		defer pf.Stop()

		time.Sleep(MetricsCollectionDelay)

		client := &http.Client{Timeout: HTTPClientTimeout}
		req, err := http.NewRequest("GET", fmt.Sprintf("http://localhost:%d/metrics", ExtensionServerMetricsPort), nil)
		Expect(err).NotTo(HaveOccurred())

		resp, err := client.Do(req)
		Expect(err).NotTo(HaveOccurred())
		defer resp.Body.Close()

		Expect(resp.StatusCode).To(Equal(http.StatusOK))

		body, err := io.ReadAll(resp.Body)
		Expect(err).NotTo(HaveOccurred())
		metricsBody := string(body)

		Expect(metricsBody).To(ContainSubstring("# HELP"))
		Expect(metricsBody).To(ContainSubstring("# TYPE"))
		Expect(metricsBody).To(ContainSubstring("grpc_requests_total"))
		Expect(metricsBody).To(ContainSubstring("grpc_request_duration_seconds"))

		initialValue := parseMetricValue(metricsBody, "xds_backend_post_cluster_modify_total")

		Expect(applyTemplate(ctx, k8sClient, "xds-backend.yaml", XdsBackendTemplate{
			XdsBackendGroup:        XdsBackendGroup,
			XdsBackendAPIVersion:   XdsBackendAPIVersion,
			XdsBackendKind:         XdsBackendKind,
			XdsBackendResourceName: "test-xds-backend-metrics",
			EnvoyGatewayNamespace:  EnvoyGatewayNamespace,
			EdsConfigPath:          FileXdsEdsConfigPath,
			TestServiceName:        TestServiceName,
		})).To(Succeed())

		Expect(applyTemplate(ctx, k8sClient, "httproute.yaml", HTTPRouteTemplate{
			HTTPRouteName:          "test-httproute-metrics",
			GatewayName:            GatewayName,
			EnvoyGatewayNamespace:  EnvoyGatewayNamespace,
			XdsBackendGroup:        XdsBackendGroup,
			XdsBackendKind:         XdsBackendKind,
			XdsBackendResourceName: "test-xds-backend-metrics",
			HTTPRoutePathPrefix:    "/metrics-test",
		})).To(Succeed())

		Expect(k8sClient.WaitForHTTPRouteReady(ctx, EnvoyGatewayNamespace, "test-httproute-metrics", DeploymentTimeout)).To(Succeed())

		time.Sleep(MetricsCollectionDelay)

		req2, err := http.NewRequest("GET", fmt.Sprintf("http://localhost:%d/metrics", ExtensionServerMetricsPort), nil)
		Expect(err).NotTo(HaveOccurred())
		resp2, err := client.Do(req2)
		Expect(err).NotTo(HaveOccurred())
		defer resp2.Body.Close()
		Expect(resp2.StatusCode).To(Equal(http.StatusOK))

		body2, err := io.ReadAll(resp2.Body)
		Expect(err).NotTo(HaveOccurred())
		metricsBody2 := string(body2)

		finalValue := parseMetricValue(metricsBody2, "xds_backend_post_cluster_modify_total")
		Expect(finalValue).To(BeNumerically(">", initialValue), "xds_backend_post_cluster_modify_total should have incremented after creating Gateway/HTTPRoute/XdsBackend")
	})

	It("should route traffic through Envoy Gateway to test service via filexds EDS server", func() {
		k8sClient, err := NewK8sClient(cluster.GetKubeconfigPath())
		Expect(err).NotTo(HaveOccurred())

		Expect(applyTemplate(ctx, k8sClient, "xds-backend-filexds.yaml", XdsBackendFileXdsTemplate{
			XdsBackendGroup:        XdsBackendGroup,
			XdsBackendAPIVersion:   XdsBackendAPIVersion,
			XdsBackendKind:         XdsBackendKind,
			XdsBackendResourceName: FileXdsXdsBackendResourceName,
			EnvoyGatewayNamespace:  EnvoyGatewayNamespace,
			TestServiceName:        TestServiceName,
			FileXdsClusterName:     FileXdsClusterName,
		})).To(Succeed())

		Expect(applyTemplate(ctx, k8sClient, "httproute.yaml", HTTPRouteTemplate{
			HTTPRouteName:          FileXdsHTTPRouteName,
			GatewayName:            GatewayName,
			EnvoyGatewayNamespace:  EnvoyGatewayNamespace,
			XdsBackendGroup:        XdsBackendGroup,
			XdsBackendKind:         XdsBackendKind,
			XdsBackendResourceName: FileXdsXdsBackendResourceName,
			HTTPRoutePathPrefix:    HTTPRoutePathPrefixEds,
		})).To(Succeed())

		Expect(k8sClient.WaitForHTTPRouteReady(ctx, EnvoyGatewayNamespace, FileXdsHTTPRouteName, DeploymentTimeout)).To(Succeed())
		labelSelector := fmt.Sprintf("%s=%s,%s=%s", EnvoyProxyOwningGatewayLabelKey, GatewayName, EnvoyProxyComponentLabelKey, EnvoyProxyComponentLabelValue)
		pods, err := k8sClient.GetClientset().CoreV1().Pods(EnvoyGatewayNamespace).List(ctx, metav1.ListOptions{LabelSelector: labelSelector})
		Expect(err).NotTo(HaveOccurred())
		Expect(len(pods.Items)).To(BeNumerically(">", 0))
		podName := pods.Items[0].Name

		Expect(wait.PollUntilContextTimeout(ctx, TestPollInterval, DeploymentTimeout, true, func(ctx context.Context) (bool, error) {
			configDump, err := getEnvoyAdminConfigDump(ctx, k8sClient, EnvoyGatewayNamespace, podName)
			if err != nil {
				return false, nil
			}
			return strings.Contains(configDump, FileXdsExpectedClusterName), nil
		})).To(Succeed())

		time.Sleep(EnvoyEndpointProcessingDelay)
		restConfig := k8sClient.GetConfig()
		portMapping := fmt.Sprintf("%d:%d", EnvoyProxyPodPort, EnvoyProxyPodPort)
		pf, err := SetupPortForward(ctx, restConfig, EnvoyGatewayNamespace, podName, []string{portMapping})
		Expect(err).NotTo(HaveOccurred())
		defer pf.Stop()

		var resp *http.Response
		Expect(wait.PollUntilContextTimeout(ctx, TestPollInterval, HTTPRequestTimeout, true, func(ctx context.Context) (bool, error) {
			client := &http.Client{Timeout: HTTPClientTimeout}
			req, err := http.NewRequestWithContext(ctx, "GET", fmt.Sprintf("http://localhost:%d%s", EnvoyProxyPodPort, HTTPRoutePathPrefixEds), nil)
			if err != nil {
				return false, err
			}
			req.Header.Set("Host", "*")

			attemptResp, err := client.Do(req)
			if err != nil {
				return false, nil
			}

			if attemptResp.StatusCode == http.StatusOK {
				resp = attemptResp
				return true, nil
			}
			attemptResp.Body.Close()
			return false, nil
		})).To(Succeed())
		defer resp.Body.Close()
		Expect(resp.StatusCode).To(Equal(http.StatusOK))

		time.Sleep(EnvoyAccessLogFlushDelay)
	})

	It("should connect to extension server over TLS endpoint", func() {
		k8sClient, err := NewK8sClient(cluster.GetKubeconfigPath())
		Expect(err).NotTo(HaveOccurred())

		tlsSecretName := "xds-backend-tls"
		secret, err := k8sClient.GetClientset().CoreV1().Secrets(ExtensionServerNamespace).Get(ctx, tlsSecretName, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		certPEM := secret.Data["tls.crt"]
		Expect(certPEM).NotTo(BeEmpty())

		tlsConfig, err := CreateTLSConfig(certPEM)
		Expect(err).NotTo(HaveOccurred())

		pods, err := k8sClient.GetClientset().CoreV1().Pods(ExtensionServerNamespace).List(ctx, metav1.ListOptions{
			LabelSelector: ExtensionServerLabelSelector,
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(len(pods.Items)).To(BeNumerically(">", 0))
		podName := pods.Items[0].Name

		restConfig := k8sClient.GetConfig()
		localPort := 5007
		portMapping := fmt.Sprintf("%d:%d", localPort, ExtensionServerTLSPort)
		pf, err := SetupPortForward(ctx, restConfig, ExtensionServerNamespace, podName, []string{portMapping})
		Expect(err).NotTo(HaveOccurred())
		defer pf.Stop()

		conn, err := grpc.NewClient(
			fmt.Sprintf("localhost:%d", localPort),
			grpc.WithTransportCredentials(credentials.NewTLS(tlsConfig)),
		)
		Expect(err).NotTo(HaveOccurred())
		defer conn.Close()

		healthClient := grpc_health_v1.NewHealthClient(conn)
		healthResp, err := healthClient.Check(ctx, &grpc_health_v1.HealthCheckRequest{})
		Expect(err).NotTo(HaveOccurred())
		Expect(healthResp.Status).To(Equal(grpc_health_v1.HealthCheckResponse_SERVING))
	})

	It("should support BackendTLSPolicy for xDS backend", func() {
		if !runExperimental {
			Skip("Skipping experimental test (use -experimental flag to enable)")
		}

		k8sClient, err := NewK8sClient(cluster.GetKubeconfigPath())
		Expect(err).NotTo(HaveOccurred())

		tlsSecret, err := k8sClient.GetClientset().CoreV1().Secrets(TestNamespace).Get(ctx, TestServiceTLSSecretName, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		backendCertPEM := tlsSecret.Data["tls.crt"]
		Expect(backendCertPEM).NotTo(BeEmpty())

		caCertPEM := string(backendCertPEM)
		Expect(caCertPEM).NotTo(BeEmpty(), "CA certificate PEM should not be empty")
		Expect(applyTemplate(ctx, k8sClient, "backend-tls-ca-configmap.yaml", BackendTLSCACertConfigMapTemplate{
			BackendTLSCACertName:  BackendTLSCACertName,
			EnvoyGatewayNamespace: EnvoyGatewayNamespace,
			BackendTLSCACertPEM:   caCertPEM,
		})).To(Succeed())

		Expect(applyTemplate(ctx, k8sClient, "xds-backend-filexds.yaml", XdsBackendFileXdsTemplate{
			XdsBackendGroup:        XdsBackendGroup,
			XdsBackendAPIVersion:   XdsBackendAPIVersion,
			XdsBackendKind:         XdsBackendKind,
			XdsBackendResourceName: BackendTLSPolicyXdsBackendName,
			EnvoyGatewayNamespace:  EnvoyGatewayNamespace,
			FileXdsClusterName:     FileXdsClusterName,
			TestServiceName:        fmt.Sprintf("%s-tls", TestServiceName),
		})).To(Succeed())

		Expect(applyTemplate(ctx, k8sClient, "backendtlspolicy.yaml", BackendTLSPolicyTemplate{
			BackendTLSPolicyName:   BackendTLSPolicyName,
			EnvoyGatewayNamespace:  EnvoyGatewayNamespace,
			XdsBackendGroup:        XdsBackendGroup,
			XdsBackendKind:         XdsBackendKind,
			XdsBackendResourceName: BackendTLSPolicyXdsBackendName,
			BackendTLSHostname:     BackendTLSHostname,
			BackendTLSCACertName:   BackendTLSCACertName,
		})).To(Succeed())

		Expect(applyTemplate(ctx, k8sClient, "httproute.yaml", HTTPRouteTemplate{
			HTTPRouteName:          BackendTLSPolicyHTTPRouteName,
			GatewayName:            GatewayName,
			EnvoyGatewayNamespace:  EnvoyGatewayNamespace,
			XdsBackendGroup:        XdsBackendGroup,
			XdsBackendKind:         XdsBackendKind,
			XdsBackendResourceName: BackendTLSPolicyXdsBackendName,
			HTTPRoutePathPrefix:    BackendTLSPolicyPathPrefix,
		})).To(Succeed())

		By("Waiting for HTTPRoute to be ready")
		Expect(k8sClient.WaitForHTTPRouteReady(ctx, EnvoyGatewayNamespace, BackendTLSPolicyHTTPRouteName, DeploymentTimeout)).To(Succeed())

		By("Checking HTTPRoute status")
		route, err := k8sClient.GetHTTPRoute(ctx, EnvoyGatewayNamespace, BackendTLSPolicyHTTPRouteName)
		Expect(err).NotTo(HaveOccurred())
		Expect(len(route.Status.Parents)).To(BeNumerically(">", 0), "HTTPRoute should have at least one parent status")

		allParentsAccepted := false
		for i, parentStatus := range route.Status.Parents {
			parentAccepted := true
			for _, condition := range parentStatus.Conditions {
				defaultLogger.Logf("HTTPRoute parent[%d] condition: type=%s, status=%s, reason=%s, message=%s", i, condition.Type, condition.Status, condition.Reason, condition.Message)
				if condition.Status == metav1.ConditionFalse {
					parentAccepted = false
				}
			}

			if parentAccepted && len(parentStatus.Conditions) > 0 {
				allParentsAccepted = true
			}
		}

		Expect(allParentsAccepted).To(BeTrue(), "HTTPRoute should have at least one parent with all conditions True")

		By("Waiting for BackendTLSPolicy to be ready")
		backendTLSPolicyGVK := schema.GroupVersionKind{
			Group:   "gateway.networking.k8s.io",
			Version: "v1",
			Kind:    "BackendTLSPolicy",
		}
		backendTLSPolicyGVR := schema.GroupVersionResource{
			Group:    backendTLSPolicyGVK.Group,
			Version:  backendTLSPolicyGVK.Version,
			Resource: "backendtlspolicies",
		}

		Expect(wait.PollUntilContextTimeout(ctx, TestPollInterval, DeploymentTimeout, true, func(ctx context.Context) (bool, error) {
			resourceClient, err := k8sClient.GetDynamicClient().Resource(backendTLSPolicyGVR).Namespace(EnvoyGatewayNamespace).Get(ctx, BackendTLSPolicyName, metav1.GetOptions{})
			if err != nil {
				return false, err
			}
			status, found, _ := unstructured.NestedMap(resourceClient.Object, "status")
			if !found {
				defaultLogger.Logf("BackendTLSPolicy status not found yet")
				return false, nil
			}
			ancestors, found, _ := unstructured.NestedSlice(status, "ancestors")
			if !found || len(ancestors) == 0 {
				defaultLogger.Logf("BackendTLSPolicy ancestors not found yet")
				return false, nil
			}

			hasValidAncestor := false
			for i, ancestor := range ancestors {
				ancestorMap, ok := ancestor.(map[string]interface{})
				if !ok {
					continue
				}

				ancestorName, _ := ancestorMap["name"].(string)
				ancestorNamespace, _ := ancestorMap["namespace"].(string)
				ancestorKind, _ := ancestorMap["kind"].(string)

				conditions, found, _ := unstructured.NestedSlice(ancestorMap, "conditions")
				if !found {
					defaultLogger.Logf("BackendTLSPolicy ancestor[%d] (%s/%s/%s) has no conditions", i, ancestorKind, ancestorNamespace, ancestorName)
					continue
				}

				allConditionsTrue := true
				for _, condition := range conditions {
					condMap, ok := condition.(map[string]interface{})
					if !ok {
						continue
					}
					statusVal, _ := condMap["status"].(string)
					typeVal, _ := condMap["type"].(string)
					reasonVal, _ := condMap["reason"].(string)
					messageVal, _ := condMap["message"].(string)
					defaultLogger.Logf("BackendTLSPolicy ancestor[%d] (%s/%s/%s) condition: type=%s, status=%s, reason=%s, message=%s", i, ancestorKind, ancestorNamespace, ancestorName, typeVal, statusVal, reasonVal, messageVal)
					if statusVal == "False" {
						allConditionsTrue = false
					}
				}

				if allConditionsTrue && len(conditions) > 0 {
					hasValidAncestor = true
				}
			}

			if !hasValidAncestor {
				return false, nil
			}

			return true, nil
		})).To(Succeed(), "BackendTLSPolicy should be processed and have status after HTTPRoute is ready")

		labelSelector := fmt.Sprintf("%s=%s,%s=%s", EnvoyProxyOwningGatewayLabelKey, GatewayName, EnvoyProxyComponentLabelKey, EnvoyProxyComponentLabelValue)
		pods, err := k8sClient.GetClientset().CoreV1().Pods(EnvoyGatewayNamespace).List(ctx, metav1.ListOptions{LabelSelector: labelSelector})
		Expect(err).NotTo(HaveOccurred())
		Expect(len(pods.Items)).To(BeNumerically(">", 0))
		podName := pods.Items[0].Name

		expectedClusterName := fmt.Sprintf("httproute/%s/%s/rule/0", EnvoyGatewayNamespace, BackendTLSPolicyHTTPRouteName)
		var lastConfigDump string
		Expect(wait.PollUntilContextTimeout(ctx, TestPollInterval, DeploymentTimeout, true, func(ctx context.Context) (bool, error) {
			configDump, err := getEnvoyAdminConfigDump(ctx, k8sClient, EnvoyGatewayNamespace, podName)
			if err != nil {
				return false, nil
			}
			lastConfigDump = configDump
			return strings.Contains(configDump, expectedClusterName), nil
		})).To(Succeed(), func() string {
			clusters := extractClusterNames(lastConfigDump)
			return fmt.Sprintf("Expected cluster %s not found. Available clusters: %v", expectedClusterName, clusters)
		})

		restConfig := k8sClient.GetConfig()
		portMapping := fmt.Sprintf("%d:%d", EnvoyProxyPodPort, EnvoyProxyPodPort)
		pf, err := SetupPortForward(ctx, restConfig, EnvoyGatewayNamespace, podName, []string{portMapping})
		Expect(err).NotTo(HaveOccurred())
		defer pf.Stop()

		var resp *http.Response
		Expect(wait.PollUntilContextTimeout(ctx, TestPollInterval, HTTPRequestTimeout, true, func(ctx context.Context) (bool, error) {
			client := &http.Client{Timeout: HTTPClientTimeout}
			req, err := http.NewRequestWithContext(ctx, "GET", fmt.Sprintf("http://localhost:%d%s", EnvoyProxyPodPort, BackendTLSPolicyPathPrefix), nil)
			if err != nil {
				return false, err
			}
			req.Header.Set("Host", "*")

			attemptResp, err := client.Do(req)
			if err != nil {
				return false, nil
			}

			if attemptResp.StatusCode == http.StatusOK {
				resp = attemptResp
				return true, nil
			}
			// Close body and retry on 503 or other non-OK status codes
			attemptResp.Body.Close()
			return false, nil
		})).To(Succeed())
		defer resp.Body.Close()
		Expect(resp.StatusCode).To(Equal(http.StatusOK))

		time.Sleep(EnvoyAccessLogFlushDelay)
	})

	It("should support inline TLS configuration for xDS backend", func() {
		k8sClient, err := NewK8sClient(cluster.GetKubeconfigPath())
		Expect(err).NotTo(HaveOccurred())

		runTLSTest(ctx, k8sClient, TLSTestConfig{
			TestName:          "inline TLS with CA certificate and hostname",
			XdsBackendName:    InlineTLSXdsBackendName,
			HTTPRouteName:     InlineTLSHTTPRouteName,
			PathPrefix:        InlineTLSPathPrefix,
			TestServiceName:   fmt.Sprintf("%s-tls", TestServiceName),
			TlsEnabled:        true,
			TlsCaCertName:     InlineTLSCACertResourceName,
			TlsHostname:       InlineTLSHostname,
			ExpectTLSInConfig: true,
			ExpectHTTPSuccess: true,
		})
	})

	It("should handle insecure TLS configuration with empty but non-nil TLS config", func() {
		k8sClient, err := NewK8sClient(cluster.GetKubeconfigPath())
		Expect(err).NotTo(HaveOccurred())

		runTLSTest(ctx, k8sClient, TLSTestConfig{
			TestName:        "empty TLS config (tls: {})",
			XdsBackendName:  InsecureTLSXdsBackendName,
			HTTPRouteName:   InsecureTLSHTTPRouteName,
			PathPrefix:      InsecureTLSPathPrefix,
			TestServiceName: TestServiceName,
			TlsEnabled:      true,
			// No TlsCaCertName or TlsHostname provided, so outputs empty TLS config (tls: {})
			ExpectTLSInConfig: false, // Empty TLS config may or may not show up in config dump
			ExpectHTTPSuccess: false, // Connection may fail due to TLS validation
		})
	})

	It("should support multiple custom backendRefs in HTTPRoute", func() {
		if !runExperimental {
			Skip("Skipping experimental test (use -experimental flag to enable)")
		}

		k8sClient, err := NewK8sClient(cluster.GetKubeconfigPath())
		Expect(err).NotTo(HaveOccurred())

		By("Creating XdsBackend resources for multiple backends")
		Expect(applyTemplate(ctx, k8sClient, "xds-backend-filexds.yaml", XdsBackendFileXdsTemplate{
			XdsBackendGroup:        XdsBackendGroup,
			XdsBackendAPIVersion:   XdsBackendAPIVersion,
			XdsBackendKind:         XdsBackendKind,
			XdsBackendResourceName: XdsBackend1Name,
			EnvoyGatewayNamespace:  EnvoyGatewayNamespace,
			FileXdsClusterName:     FileXdsClusterName,
			TestServiceName:        TestServiceName,
		})).To(Succeed())

		Expect(applyTemplate(ctx, k8sClient, "xds-backend-filexds.yaml", XdsBackendFileXdsTemplate{
			XdsBackendGroup:        XdsBackendGroup,
			XdsBackendAPIVersion:   XdsBackendAPIVersion,
			XdsBackendKind:         XdsBackendKind,
			XdsBackendResourceName: XdsBackend2Name,
			EnvoyGatewayNamespace:  EnvoyGatewayNamespace,
			FileXdsClusterName:     FileXdsClusterName,
			TestServiceName:        TestService2Name,
		})).To(Succeed())

		By("Creating HTTPRoute with multiple backendRefs")
		Expect(applyTemplate(ctx, k8sClient, "httproute.yaml", HTTPRouteTemplate{
			HTTPRouteName:         MultipleBackendRefsHTTPRouteName,
			GatewayName:           GatewayName,
			EnvoyGatewayNamespace: EnvoyGatewayNamespace,
			HTTPRoutePathPrefix:   MultipleBackendRefsPathPrefix,
			BackendRefs: []BackendRef{
				{
					Group:     XdsBackendGroup,
					Kind:      XdsBackendKind,
					Name:      XdsBackend1Name,
					Namespace: EnvoyGatewayNamespace,
					Weight:    1,
				},
				{
					Group:     XdsBackendGroup,
					Kind:      XdsBackendKind,
					Name:      XdsBackend2Name,
					Namespace: EnvoyGatewayNamespace,
					Weight:    2,
				},
			},
		})).To(Succeed())

		By("Waiting for HTTPRoute to be ready")
		Expect(k8sClient.WaitForHTTPRouteReady(ctx, EnvoyGatewayNamespace, MultipleBackendRefsHTTPRouteName, DeploymentTimeout)).To(Succeed())

		By("Waiting for clusters to be created in Envoy")
		labelSelector := fmt.Sprintf("%s=%s,%s=%s", EnvoyProxyOwningGatewayLabelKey, GatewayName, EnvoyProxyComponentLabelKey, EnvoyProxyComponentLabelValue)
		pods, err := k8sClient.GetClientset().CoreV1().Pods(EnvoyGatewayNamespace).List(ctx, metav1.ListOptions{LabelSelector: labelSelector})
		Expect(err).NotTo(HaveOccurred())
		Expect(len(pods.Items)).To(BeNumerically(">", 0))
		podName := pods.Items[0].Name

		expectedClusterName := fmt.Sprintf("httproute/%s/%s/rule/0", EnvoyGatewayNamespace, MultipleBackendRefsHTTPRouteName)
		Expect(wait.PollUntilContextTimeout(ctx, TestPollInterval, DeploymentTimeout, true, func(ctx context.Context) (bool, error) {
			configDump, err := getEnvoyAdminConfigDump(ctx, k8sClient, EnvoyGatewayNamespace, podName)
			if err != nil {
				return false, nil
			}
			return strings.Contains(configDump, expectedClusterName), nil
		})).To(Succeed())

		By("Setting up port forward to Envoy proxy")
		restConfig := k8sClient.GetConfig()
		portMapping := fmt.Sprintf("%d:%d", EnvoyProxyPodPort, EnvoyProxyPodPort)
		pf, err := SetupPortForward(ctx, restConfig, EnvoyGatewayNamespace, podName, []string{portMapping})
		Expect(err).NotTo(HaveOccurred())
		defer pf.Stop()

		By("Making requests to verify both backends are reachable")
		client := &http.Client{Timeout: HTTPClientTimeout}
		responses := make(map[string]int)
		service1Response := "Hello from test service\n"
		service2Response := "Response from service 2\n"

		// Poll until we've received responses from both backends
		Expect(wait.PollUntilContextTimeout(ctx, TestPollInterval, HTTPRequestTimeout, true, func(ctx context.Context) (bool, error) {
			req, err := http.NewRequestWithContext(ctx, "GET", fmt.Sprintf("http://localhost:%d%s", EnvoyProxyPodPort, MultipleBackendRefsPathPrefix), nil)
			if err != nil {
				return false, nil // Retry on request creation error
			}
			req.Header.Set("Host", "*")

			resp, err := client.Do(req)
			if err != nil {
				return false, nil // Retry on network error
			}
			defer resp.Body.Close()

			if resp.StatusCode == http.StatusOK {
				body, err := io.ReadAll(resp.Body)
				if err == nil {
					responseText := string(body)
					responses[responseText] = responses[responseText] + 1
				}
			}

			// Check if we've received responses from both backends
			service1Count := responses[service1Response]
			service2Count := responses[service2Response]
			if service1Count > 0 && service2Count > 0 {
				return true, nil // Both backends responded
			}

			// Small delay between requests
			time.Sleep(100 * time.Millisecond)
			return false, nil // Continue polling
		})).To(Succeed(), func() string {
			service1Count := responses[service1Response]
			service2Count := responses[service2Response]
			return fmt.Sprintf("Expected responses from both backends, but got: service-1=%d, service-2=%d. All responses: %v", service1Count, service2Count, responses)
		})

		By("Verifying both responses were detected")
		service1Count := responses[service1Response]
		service2Count := responses[service2Response]

		defaultLogger.Logf("Response counts - service-1: %d, service-2: %d", service1Count, service2Count)
		defaultLogger.Logf("All responses: %v", responses)

		Expect(service1Count).To(BeNumerically(">", 0), "Should have received at least one response from service-1")
		Expect(service2Count).To(BeNumerically(">", 0), "Should have received at least one response from service-2")

		time.Sleep(EnvoyAccessLogFlushDelay)
	})
})

func applyTemplate(ctx context.Context, k8sClient *K8sClient, templateName string, data interface{}) error {
	yaml, err := LoadTemplate(templateName, data)
	if err != nil {
		return fmt.Errorf("failed to load template %s: %w", templateName, err)
	}
	return k8sClient.ApplyYAML(ctx, yaml)
}

func getEnvoyAdminEndpoint(ctx context.Context, k8sClient *K8sClient, namespace, podName, adminPath string) (string, error) {
	adminPort := EnvoyAdminPort
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

	select {
	case <-ctx.Done():
		return "", fmt.Errorf("context cancelled: %w", ctx.Err())
	case err := <-errChan:
		return "", fmt.Errorf("port forward error: %w", err)
	case <-readyChan:
		time.Sleep(EnvoyAdminAPIReadyDelay)
	}

	adminURL := fmt.Sprintf("http://localhost:%d%s", EnvoyAdminPortForwardPort, adminPath)
	client := &http.Client{Timeout: HTTPClientTimeout}
	resp, err := client.Get(adminURL)
	if err != nil {
		return "", fmt.Errorf("failed to get %s: %w", adminPath, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("admin API %s returned status %d", adminPath, resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read response: %w", err)
	}

	return string(body), nil
}

func getEnvoyAdminConfigDump(ctx context.Context, k8sClient *K8sClient, namespace, podName string) (string, error) {
	body, err := getEnvoyAdminEndpoint(ctx, k8sClient, namespace, podName, EnvoyAdminConfigDumpPath)
	if err != nil {
		return "", err
	}

	// Parse JSON and convert to YAML
	var configDump interface{}
	if err := json.Unmarshal([]byte(body), &configDump); err != nil {
		// If JSON parsing fails, return raw body
		return body, nil
	}

	// Convert to YAML
	yamlBytes, err := yaml.Marshal(configDump)
	if err != nil {
		// If YAML conversion fails, return JSON pretty-printed
		jsonBytes, jsonErr := json.MarshalIndent(configDump, "", "  ")
		if jsonErr != nil {
			return body, nil
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

// getExtensionServerLogs fetches the extension server logs from pods, optionally filtered by time
func getExtensionServerLogs(ctx context.Context, k8sClient *K8sClient, namespace string, sinceTime *metav1.Time) (string, error) {
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

		opts := &corev1.PodLogOptions{
			Container: containerName,
		}
		if sinceTime != nil {
			opts.SinceTime = sinceTime
		}

		logs, err := k8sClient.GetClientset().CoreV1().Pods(namespace).GetLogs(pod.Name, opts).Stream(ctx)
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

// getFileXdsServerLogs fetches the filexds server logs from pods, optionally filtered by time
func getFileXdsServerLogs(ctx context.Context, k8sClient *K8sClient, namespace string, sinceTime *metav1.Time) (string, error) {
	// Find filexds server pods
	pods, err := k8sClient.GetClientset().CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("app=%s", FileXdsDeploymentName),
	})
	if err != nil {
		return "", fmt.Errorf("failed to list pods: %w", err)
	}

	if len(pods.Items) == 0 {
		return "No filexds server pods found", nil
	}

	var allLogs []string
	for _, pod := range pods.Items {
		// Container name is "filexds"
		containerName := "filexds"
		if len(pod.Spec.Containers) > 0 {
			containerName = pod.Spec.Containers[0].Name
		}

		opts := &corev1.PodLogOptions{
			Container: containerName,
		}
		if sinceTime != nil {
			opts.SinceTime = sinceTime
		}

		logs, err := k8sClient.GetClientset().CoreV1().Pods(namespace).GetLogs(pod.Name, opts).Stream(ctx)
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

// getEnvoyGatewayControllerLogs fetches the Envoy Gateway controller logs from pods, optionally filtered by time
func getEnvoyGatewayControllerLogs(ctx context.Context, k8sClient *K8sClient, namespace string, sinceTime *metav1.Time) (string, error) {
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

		opts := &corev1.PodLogOptions{
			Container: containerName,
		}
		if sinceTime != nil {
			opts.SinceTime = sinceTime
		}

		logs, err := k8sClient.GetClientset().CoreV1().Pods(namespace).GetLogs(pod.Name, opts).Stream(ctx)
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
	// Get the directory of the current file (integration_test.go)
	_, callerFile, _, _ := runtime.Caller(0)
	baseDir := filepath.Dir(callerFile)

	// Get current test name from Ginkgo
	testName := "setup"
	spec := CurrentSpecReport()
	if spec.FullText() != "" {
		// Sanitize test name for filesystem
		testName = sanitizeTestName(spec.FullText())
	}

	// Create subdirectory for this test
	logsDir := filepath.Join(baseDir, LogDir, testName)
	if err := os.MkdirAll(logsDir, 0755); err != nil {
		logsDir = filepath.Join(os.TempDir(), "xds-backend-e2e-logs", testName)
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

// getTestServiceLogs fetches the test HTTP service logs from pods, optionally filtered by time
func getTestServiceLogs(ctx context.Context, k8sClient *K8sClient, namespace string, sinceTime *metav1.Time) (string, error) {
	// Find test service pods
	pods, err := k8sClient.GetClientset().CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("app=%s", TestServiceName),
	})
	if err != nil {
		return "", fmt.Errorf("failed to list pods: %w", err)
	}

	if len(pods.Items) == 0 {
		return "No test service pods found", nil
	}

	var allLogs []string
	for _, pod := range pods.Items {
		containerName := TestServiceName
		if len(pod.Spec.Containers) > 0 {
			containerName = pod.Spec.Containers[0].Name
		}

		opts := &corev1.PodLogOptions{
			Container: containerName,
		}
		if sinceTime != nil {
			opts.SinceTime = sinceTime
		}

		logs, err := k8sClient.GetClientset().CoreV1().Pods(namespace).GetLogs(pod.Name, opts).Stream(ctx)
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

func getEnvoyGatewayAdminConfigDump(ctx context.Context, k8sClient *K8sClient, namespace string) (string, error) {
	pods, err := k8sClient.GetClientset().CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: EnvoyGatewayLabelSelector,
	})
	if err != nil {
		return "", fmt.Errorf("failed to list pods: %w", err)
	}

	if len(pods.Items) == 0 {
		return "", fmt.Errorf("no Envoy Gateway controller pods found")
	}

	podName := pods.Items[0].Name
	adminPort := 19000
	adminPath := "/api/config_dump"
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

	localPort := 19002
	adminPortMapping := fmt.Sprintf("%d:%d", localPort, adminPort)
	fw, err := portforward.New(dialer, []string{adminPortMapping}, stopChan, readyChan, nil, nil)
	if err != nil {
		return "", fmt.Errorf("failed to create port forward: %w", err)
	}

	go func() {
		if err := fw.ForwardPorts(); err != nil {
			select {
			case errChan <- err:
			default:
			}
		}
	}()
	defer close(stopChan)

	select {
	case <-ctx.Done():
		return "", fmt.Errorf("context cancelled: %w", ctx.Err())
	case err := <-errChan:
		return "", fmt.Errorf("port forward error: %w", err)
	case <-readyChan:
		time.Sleep(EnvoyAdminAPIReadyDelay)
	}

	adminURL := fmt.Sprintf("http://localhost:%d%s", localPort, adminPath)
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

	return string(body), nil
}

func collectLogs(ctx context.Context, k8sClient *K8sClient, testStartTime time.Time) {
	sinceTime := metav1.NewTime(testStartTime)
	if extensionLogs, err := getExtensionServerLogs(ctx, k8sClient, ExtensionServerNamespace, &sinceTime); err == nil {
		writeLogToFile("extension-server", extensionLogs)
	}
	if fileXdsLogs, err := getFileXdsServerLogs(ctx, k8sClient, EnvoyGatewayNamespace, &sinceTime); err == nil {
		writeLogToFile("filexds-server", fileXdsLogs)
	}
	if testServiceLogs, err := getTestServiceLogs(ctx, k8sClient, TestNamespace, &sinceTime); err == nil {
		writeLogToFile("test-service", testServiceLogs)
	}
	if egControllerLogs, err := getEnvoyGatewayControllerLogs(ctx, k8sClient, EnvoyGatewayNamespace, &sinceTime); err == nil {
		writeLogToFile("envoy-gateway-controller", egControllerLogs)
	}
	if egAdminConfigDump, err := getEnvoyGatewayAdminConfigDump(ctx, k8sClient, EnvoyGatewayNamespace); err == nil {
		writeLogToFile("envoy-gateway-admin-config-dump", egAdminConfigDump)
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

// TLSTestConfig holds configuration for TLS tests
type TLSTestConfig struct {
	TestName          string
	XdsBackendName    string
	HTTPRouteName     string
	PathPrefix        string
	TestServiceName   string
	TlsEnabled        bool
	TlsCaCertName     string
	TlsCaCertEmpty    bool
	TlsHostname       string
	ExpectTLSInConfig bool // Whether to expect TLS configuration in Envoy config dump
	ExpectHTTPSuccess bool // Whether to expect successful HTTP requests (default: true)
}

// runTLSTest is a helper function to run TLS configuration tests
func runTLSTest(ctx context.Context, k8sClient *K8sClient, config TLSTestConfig) {
	By(fmt.Sprintf("Creating XdsBackend: %s", config.TestName))
	Expect(applyTemplate(ctx, k8sClient, "xds-backend-filexds.yaml", XdsBackendFileXdsTemplate{
		XdsBackendGroup:        XdsBackendGroup,
		XdsBackendAPIVersion:   XdsBackendAPIVersion,
		XdsBackendKind:         XdsBackendKind,
		XdsBackendResourceName: config.XdsBackendName,
		EnvoyGatewayNamespace:  EnvoyGatewayNamespace,
		FileXdsClusterName:     FileXdsClusterName,
		TestServiceName:        config.TestServiceName,
		TlsEnabled:             config.TlsEnabled,
		TlsCaCertName:          config.TlsCaCertName,
		TlsCaCertEmpty:         config.TlsCaCertEmpty,
		TlsHostname:            config.TlsHostname,
	})).To(Succeed())

	By(fmt.Sprintf("Creating HTTPRoute: %s", config.TestName))
	Expect(applyTemplate(ctx, k8sClient, "httproute.yaml", HTTPRouteTemplate{
		HTTPRouteName:          config.HTTPRouteName,
		GatewayName:            GatewayName,
		EnvoyGatewayNamespace:  EnvoyGatewayNamespace,
		XdsBackendGroup:        XdsBackendGroup,
		XdsBackendKind:         XdsBackendKind,
		XdsBackendResourceName: config.XdsBackendName,
		HTTPRoutePathPrefix:    config.PathPrefix,
	})).To(Succeed())

	By("Waiting for HTTPRoute to be ready")
	Expect(k8sClient.WaitForHTTPRouteReady(ctx, EnvoyGatewayNamespace, config.HTTPRouteName, DeploymentTimeout)).To(Succeed())

	By("Checking cluster configuration in Envoy")
	labelSelector := fmt.Sprintf("%s=%s,%s=%s", EnvoyProxyOwningGatewayLabelKey, GatewayName, EnvoyProxyComponentLabelKey, EnvoyProxyComponentLabelValue)
	pods, err := k8sClient.GetClientset().CoreV1().Pods(EnvoyGatewayNamespace).List(ctx, metav1.ListOptions{LabelSelector: labelSelector})
	Expect(err).NotTo(HaveOccurred())
	Expect(len(pods.Items)).To(BeNumerically(">", 0))
	podName := pods.Items[0].Name

	expectedClusterName := fmt.Sprintf("httproute/%s/%s/rule/0", EnvoyGatewayNamespace, config.HTTPRouteName)
	var lastConfigDump string
	Expect(wait.PollUntilContextTimeout(ctx, TestPollInterval, DeploymentTimeout, true, func(ctx context.Context) (bool, error) {
		configDump, err := getEnvoyAdminConfigDump(ctx, k8sClient, EnvoyGatewayNamespace, podName)
		if err != nil {
			return false, nil
		}
		lastConfigDump = configDump
		if !strings.Contains(configDump, expectedClusterName) {
			return false, nil
		}
		if config.ExpectTLSInConfig {
			clusterHasTLS := strings.Contains(configDump, "envoy.transport_sockets.tls")
			return clusterHasTLS, nil
		}
		return true, nil
	})).To(Succeed(), func() string {
		clusters := extractClusterNames(lastConfigDump)
		return fmt.Sprintf("Expected cluster %s not found. Available clusters: %v", expectedClusterName, clusters)
	})

	// Test HTTP connectivity if ExpectHTTPSuccess is true (default)
	if config.ExpectHTTPSuccess {
		By("Testing HTTP connectivity")
		restConfig := k8sClient.GetConfig()
		portMapping := fmt.Sprintf("%d:%d", EnvoyProxyPodPort, EnvoyProxyPodPort)
		pf, err := SetupPortForward(ctx, restConfig, EnvoyGatewayNamespace, podName, []string{portMapping})
		Expect(err).NotTo(HaveOccurred())
		defer pf.Stop()

		var resp *http.Response
		var lastErr error
		var lastStatusCode int
		tlsClientTimeout := 30 * time.Second
		Expect(wait.PollUntilContextTimeout(ctx, TestPollInterval, HTTPRequestTimeout, true, func(ctx context.Context) (bool, error) {
			client := &http.Client{Timeout: tlsClientTimeout}
			req, err := http.NewRequestWithContext(ctx, "GET", fmt.Sprintf("http://localhost:%d%s", EnvoyProxyPodPort, config.PathPrefix), nil)
			if err != nil {
				lastErr = err
				return false, nil
			}
			req.Header.Set("Host", "*")

			attemptResp, err := client.Do(req)
			if err != nil {
				lastErr = err
				defaultLogger.Logf("HTTP request attempt failed: %v", err)
				return false, nil
			}
			lastStatusCode = attemptResp.StatusCode

			if attemptResp.StatusCode == http.StatusOK {
				resp = attemptResp
				return true, nil
			}
			attemptResp.Body.Close()
			defaultLogger.Logf("HTTP request returned status code %d, retrying...", lastStatusCode)
			return false, nil
		})).To(Succeed(), func() string {
			if lastErr != nil {
				return fmt.Sprintf("HTTP request failed with error: %v", lastErr)
			}
			return fmt.Sprintf("HTTP request returned status code %d instead of 200", lastStatusCode)
		})
		if resp != nil {
			defer resp.Body.Close()
			Expect(resp.StatusCode).To(Equal(http.StatusOK))
		}
	}

	time.Sleep(EnvoyAccessLogFlushDelay)
}
