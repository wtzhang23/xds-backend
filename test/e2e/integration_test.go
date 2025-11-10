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
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("xDS Backend Integration", func() {

	BeforeEach(func() {
		// xDS server, test service, and extension server configuration are already set up in BeforeSuite
		// Just verify everything is ready
		defaultLogger.Log("\n[IntegrationTest] === Verifying test environment ===")
		Expect(cluster).NotTo(BeNil(), "cluster should be initialized in BeforeSuite")
		Expect(xdsServer).NotTo(BeNil(), "xDS server should be initialized in BeforeSuite")
		Expect(testService).NotTo(BeNil(), "test service should be initialized in BeforeSuite")
		defaultLogger.Log("[IntegrationTest] Test environment is ready\n")
	})

	AfterEach(func() {
		// Collect logs for debugging, regardless of test outcome
		defaultLogger.Log("\n[IntegrationTest] === Collecting logs for debugging ===")
		k8sClient, err := NewK8sClient(cluster.GetKubeconfigPath())
		if err != nil {
			defaultLogger.Logf("[IntegrationTest] Warning: Failed to create K8s client for log collection: %v", err)
			return
		}

		// Use a context with timeout for log collection
		logCtx, logCancel := context.WithTimeout(context.Background(), LogExtractionTimeout)
		defer logCancel()

		// Collect extension server logs
		defaultLogger.Log("[IntegrationTest] Collecting extension server logs...")
		extensionLogs, extLogErr := getExtensionServerLogs(logCtx, k8sClient, ExtensionServerNamespace)
		if extLogErr == nil {
			logLines := strings.Count(extensionLogs, "\n")
			filepath := writeLogToFile("extension-server", extensionLogs)
			if filepath != "" {
				defaultLogger.Logf("[IntegrationTest] Extension server logs written to: %s (%d lines)", filepath, logLines)
			}
		}

		// Collect Envoy Gateway controller logs
		defaultLogger.Log("[IntegrationTest] Collecting Envoy Gateway controller logs...")
		egControllerLogs, egLogErr := getEnvoyGatewayControllerLogs(logCtx, k8sClient, EnvoyGatewayNamespace)
		if egLogErr == nil {
			logLines := strings.Count(egControllerLogs, "\n")
			filepath := writeLogToFile("envoy-gateway-controller", egControllerLogs)
			if filepath != "" {
				defaultLogger.Logf("[IntegrationTest] Envoy Gateway controller logs written to: %s (%d lines)", filepath, logLines)
			}
		}

		// Try to collect Envoy proxy logs and config dump if we can find a pod
		labelSelector := fmt.Sprintf("%s=%s,%s=%s", EnvoyProxyOwningGatewayLabelKey, GatewayName, EnvoyProxyComponentLabelKey, EnvoyProxyComponentLabelValue)
		pods, err := k8sClient.GetClientset().CoreV1().Pods(EnvoyGatewayNamespace).List(logCtx, metav1.ListOptions{
			LabelSelector: labelSelector,
		})
		if err == nil && len(pods.Items) > 0 {
			podName := pods.Items[0].Name

			// Collect Envoy access logs
			defaultLogger.Logf("[IntegrationTest] Collecting Envoy access logs from pod %s...", podName)
			accessLogs, logErr := getEnvoyAccessLogs(logCtx, k8sClient, EnvoyGatewayNamespace, podName)
			if logErr == nil {
				logLines := strings.Count(accessLogs, "\n")
				filepath := writeLogToFile("envoy-access", accessLogs)
				if filepath != "" {
					defaultLogger.Logf("[IntegrationTest] Envoy access logs written to: %s (%d lines)", filepath, logLines)
				}
			}

			// Collect Envoy admin config dump
			defaultLogger.Logf("[IntegrationTest] Collecting Envoy admin config dump from pod %s...", podName)
			adminConfigDump, dumpErr := getEnvoyAdminConfigDump(logCtx, k8sClient, EnvoyGatewayNamespace, podName)
			if dumpErr == nil {
				filepath := writeLogToFile("envoy-config-dump", adminConfigDump)
				if filepath != "" {
					defaultLogger.Logf("[IntegrationTest] Envoy admin config dump written to: %s", filepath)
				}
			}
		}

		defaultLogger.Log("[IntegrationTest] === Log collection complete ===\n")
	})

	It("should route traffic through Envoy Gateway to test service via xDS EDS", func() {
		defaultLogger.Log("[IntegrationTest] === Running test: route traffic through Envoy Gateway ===")
		// Create an XdsBackend resource and Gateway/HTTPRoute using typed clients
		By("Creating XdsBackend, Gateway and HTTPRoute resources")
		defaultLogger.Log("[IntegrationTest] Creating XdsBackend, Gateway and HTTPRoute resources...")
		k8sClient, err := NewK8sClient(cluster.GetKubeconfigPath())
		Expect(err).NotTo(HaveOccurred())

		// Wait for Gateway API CRDs to be installed
		By("Waiting for Gateway API CRDs to be available")
		defaultLogger.Log("[IntegrationTest] Waiting for Gateway API CRDs to be installed...")
		Expect(k8sClient.WaitForCRD(ctx, "gateways.gateway.networking.k8s.io")).To(Succeed())
		Expect(k8sClient.WaitForCRD(ctx, "httproutes.gateway.networking.k8s.io")).To(Succeed())
		Expect(k8sClient.WaitForCRD(ctx, "gatewayclasses.gateway.networking.k8s.io")).To(Succeed())

		// Note: We're configuring the EnvoyProxy bootstrap config via EnvoyGateway Helm values
		// (see envoygateway.go), so we don't need to create an EnvoyProxy resource.
		// The bootstrap config with the xDS server cluster is already applied during Helm installation.
		defaultLogger.Log("[IntegrationTest] EnvoyProxy bootstrap config is configured via EnvoyGateway Helm values")

		// Verify GatewayClassName exists, create if it doesn't
		By("Verifying GatewayClassName exists")
		defaultLogger.Logf("[IntegrationTest] Verifying GatewayClassName '%s' exists...", GatewayClassName)
		gwc, err := k8sClient.GetGatewayClass(ctx, GatewayClassName)
		if err != nil {
			if errors.IsNotFound(err) {
				defaultLogger.Logf("[IntegrationTest] GatewayClassName '%s' not found, creating it...", GatewayClassName)
				// Create GatewayClass for Envoy Gateway
				// Note: We're not using ParametersRef since we configure EnvoyProxy via Helm values
				gatewayClass := &gatewayv1.GatewayClass{
					ObjectMeta: metav1.ObjectMeta{
						Name: GatewayClassName,
					},
					Spec: gatewayv1.GatewayClassSpec{
						ControllerName: GatewayClassControllerName,
					},
				}
				// Use the gateway client through a helper method
				gatewayClassClient := k8sClient.GetGatewayClient().GatewayV1().GatewayClasses()
				_, err = gatewayClassClient.Create(ctx, gatewayClass, metav1.CreateOptions{})
				if err != nil {
					defaultLogger.Logf("[IntegrationTest] ERROR: Failed to create GatewayClass: %v", err)
					Expect(err).NotTo(HaveOccurred(), "Failed to create GatewayClass '%s'", GatewayClassName)
				}
				defaultLogger.Logf("[IntegrationTest] GatewayClassName '%s' created successfully", GatewayClassName)
			} else {
				defaultLogger.Logf("[IntegrationTest] ERROR: Failed to get GatewayClassName '%s': %v", GatewayClassName, err)
				Expect(err).NotTo(HaveOccurred(), "Failed to get GatewayClassName '%s'", GatewayClassName)
			}
		} else {
			defaultLogger.Logf("[IntegrationTest] GatewayClassName '%s' already exists: controller=%s",
				GatewayClassName, gwc.Spec.ControllerName)
			// Update GatewayClass to reference EnvoyProxy if it doesn't already
			if gwc.Spec.ParametersRef == nil || gwc.Spec.ParametersRef.Name != GatewayClassName {
				defaultLogger.Log("[IntegrationTest] Updating GatewayClass to reference EnvoyProxy...")
				gwc.Spec.ParametersRef = &gatewayv1.ParametersReference{
					Group:     gatewayv1.Group("gateway.envoyproxy.io"),
					Kind:      gatewayv1.Kind("EnvoyProxy"),
					Name:      GatewayClassName,
					Namespace: (*gatewayv1.Namespace)(ptrOf(EnvoyGatewayNamespace)),
				}
				gatewayClassClient := k8sClient.GetGatewayClient().GatewayV1().GatewayClasses()
				_, err = gatewayClassClient.Update(ctx, gwc, metav1.UpdateOptions{})
				if err != nil {
					defaultLogger.Logf("[IntegrationTest] Warning: Failed to update GatewayClass: %v", err)
				} else {
					defaultLogger.Log("[IntegrationTest] GatewayClass updated to reference EnvoyProxy")
				}
			}
		}

		// EnvoyGateway is already configured via Helm chart values during installation
		// EnvoyProxy bootstrap config is set via EnvoyProxy resource above
		defaultLogger.Log("[IntegrationTest] EnvoyGateway is already configured with extension server via Helm chart")

		// Create XdsBackend using dynamic client (custom CRD)
		xdsBackendYaml := fmt.Sprintf(`apiVersion: %s/%s
kind: %s
metadata:
  name: %s
  namespace: %s
spec:
  server:
    server: %s
  service: %s
  apiType: GRPC
`, XdsBackendGroup, XdsBackendAPIVersion, XdsBackendKind, XdsBackendResourceName, EnvoyGatewayNamespace, XdsServerName, TestServiceName)
		err = applyCustomResources(ctx, k8sClient, xdsBackendYaml)
		Expect(err).NotTo(HaveOccurred(), "Failed to create XdsBackend: %v", err)
		defaultLogger.Log("[IntegrationTest] XdsBackend created successfully")

		// Create ReferenceGrant to allow HTTPRoute in test-namespace to reference XdsBackend in envoy-gateway-system
		By("Creating ReferenceGrant")
		defaultLogger.Log("[IntegrationTest] Creating ReferenceGrant for cross-namespace reference...")
		referenceGrantYaml := fmt.Sprintf(`apiVersion: gateway.networking.k8s.io/v1beta1
kind: ReferenceGrant
metadata:
  name: allow-xds-backend-ref
  namespace: %s
spec:
  from:
  - group: gateway.networking.k8s.io
    kind: HTTPRoute
    namespace: %s
  to:
  - group: %s
    kind: %s
    name: %s
`, EnvoyGatewayNamespace, TestNamespace, XdsBackendGroup, XdsBackendKind, XdsBackendResourceName)
		err = applyCustomResources(ctx, k8sClient, referenceGrantYaml)
		Expect(err).NotTo(HaveOccurred(), "Failed to create ReferenceGrant: %v", err)
		defaultLogger.Log("[IntegrationTest] ReferenceGrant created successfully")

		// Create Gateway using typed client
		// Note: ParametersRef is in GatewayClass, not Gateway
		gatewayClassName := gatewayv1.ObjectName(GatewayClassName)
		gateway := &gatewayv1.Gateway{
			ObjectMeta: metav1.ObjectMeta{
				Name:      GatewayName,
				Namespace: EnvoyGatewayNamespace,
			},
			Spec: gatewayv1.GatewaySpec{
				GatewayClassName: gatewayClassName,
				Listeners: []gatewayv1.Listener{
					{
						Name:     gatewayv1.SectionName(GatewayListenerName),
						Protocol: gatewayv1.HTTPProtocolType,
						Port:     gatewayv1.PortNumber(GatewayListenerPort),
						AllowedRoutes: &gatewayv1.AllowedRoutes{
							Namespaces: &gatewayv1.RouteNamespaces{
								From: ptrOf(gatewayv1.NamespacesFromAll),
							},
						},
					},
				},
			},
		}
		_, err = k8sClient.CreateGateway(ctx, EnvoyGatewayNamespace, gateway)
		if err != nil && !errors.IsAlreadyExists(err) {
			Expect(err).NotTo(HaveOccurred(), "Failed to create Gateway: %v", err)
		}
		defaultLogger.Log("[IntegrationTest] Gateway created successfully")

		// Wait for Gateway to be Accepted (we'll ignore Programmed status since LoadBalancer IP may not be allocated)
		defaultLogger.Log("[IntegrationTest] Waiting for Gateway to be Accepted...")
		acceptedCtx, acceptedCancel := context.WithTimeout(ctx, DeploymentTimeout)
		defer acceptedCancel()
		err = wait.PollUntilContextTimeout(acceptedCtx, TestPollInterval, DeploymentTimeout, true, func(ctx context.Context) (bool, error) {
			gateway, gErr := k8sClient.GetGateway(ctx, EnvoyGatewayNamespace, GatewayName)
			if gErr != nil {
				return false, gErr
			}

			for _, cond := range gateway.Status.Conditions {
				if cond.Type == string(gatewayv1.GatewayConditionAccepted) {
					if cond.Status == "True" {
						defaultLogger.Logf("[IntegrationTest] Gateway is Accepted: %s", cond.Message)
						return true, nil
					}
					defaultLogger.Logf("[IntegrationTest] Gateway Accepted condition: %s, Reason: %s, Message: %s", cond.Status, cond.Reason, cond.Message)
				}
			}
			return false, nil
		})
		Expect(err).NotTo(HaveOccurred(), "Gateway should be Accepted within %v", DeploymentTimeout)

		// Note: We're not waiting for Programmed status since LoadBalancer IP may not be allocated in Kind clusters
		// The Gateway can still route traffic using the ClusterIP of the Envoy service

		// Create HTTPRoute using typed client
		httpRoute := &gatewayv1.HTTPRoute{
			ObjectMeta: metav1.ObjectMeta{
				Name:      HTTPRouteName,
				Namespace: TestNamespace,
			},
			Spec: gatewayv1.HTTPRouteSpec{
				CommonRouteSpec: gatewayv1.CommonRouteSpec{
					ParentRefs: []gatewayv1.ParentReference{
						{
							Name:      gatewayv1.ObjectName(GatewayName),
							Namespace: (*gatewayv1.Namespace)(ptrOf(EnvoyGatewayNamespace)),
						},
					},
				},
				Rules: []gatewayv1.HTTPRouteRule{
					{
						Matches: []gatewayv1.HTTPRouteMatch{
							{
								Path: &gatewayv1.HTTPPathMatch{
									Type:  (*gatewayv1.PathMatchType)(ptrOf(HTTPRoutePathMatchType)),
									Value: ptrOf(HTTPRoutePathMatchValue),
								},
							},
						},
						BackendRefs: []gatewayv1.HTTPBackendRef{
							{
								BackendRef: gatewayv1.BackendRef{
									BackendObjectReference: gatewayv1.BackendObjectReference{
										Group:     (*gatewayv1.Group)(ptrOf(XdsBackendGroup)),
										Kind:      (*gatewayv1.Kind)(ptrOf(XdsBackendKind)),
										Name:      gatewayv1.ObjectName(XdsBackendResourceName),
										Namespace: (*gatewayv1.Namespace)(ptrOf(EnvoyGatewayNamespace)),
									},
								},
							},
						},
					},
				},
			},
		}
		_, err = k8sClient.CreateHTTPRoute(ctx, TestNamespace, httpRoute)
		if err != nil && !errors.IsAlreadyExists(err) {
			Expect(err).NotTo(HaveOccurred(), "Failed to create HTTPRoute: %v", err)
		}
		defaultLogger.Log("[IntegrationTest] HTTPRoute created successfully")

		// Wait for Gateway to be ready
		By("Waiting for Gateway to be accepted and ready")
		defaultLogger.Log("[IntegrationTest] Waiting for Gateway to be accepted...")
		Expect(k8sClient.WaitForGatewayReady(ctx, EnvoyGatewayNamespace, GatewayName, DeploymentTimeout)).To(Succeed())
		defaultLogger.Log("[IntegrationTest] Gateway is ready")

		// Wait a bit for EnvoyProxy resources to be created and service to be available
		// The service is created by EnvoyProxy resources, which may take a moment
		By("Waiting for EnvoyProxy resources to be created")
		defaultLogger.Log("[IntegrationTest] Waiting for EnvoyProxy service to be available...")
		timeoutCtx, cancel := context.WithTimeout(ctx, DeploymentTimeout)
		defer cancel()
		err = wait.PollUntilContextTimeout(timeoutCtx, TestPollInterval, DeploymentTimeout, true, func(ctx context.Context) (bool, error) {
			labelSelector := fmt.Sprintf("%s=%s,%s=%s", EnvoyProxyOwningGatewayLabelKey, GatewayName, EnvoyProxyComponentLabelKey, EnvoyProxyComponentLabelValue)
			services, err := k8sClient.GetClientset().CoreV1().Services(EnvoyGatewayNamespace).List(ctx, metav1.ListOptions{
				LabelSelector: labelSelector,
			})
			if err != nil {
				return false, err
			}
			if len(services.Items) > 0 {
				defaultLogger.Logf("[IntegrationTest] EnvoyProxy service found: %s", services.Items[0].Name)
				return true, nil
			}
			return false, nil
		})
		Expect(err).NotTo(HaveOccurred(), "EnvoyProxy service should be available")

		// Wait for HTTPRoute to be ready
		By("Waiting for HTTPRoute to be accepted")
		defaultLogger.Log("[IntegrationTest] Waiting for HTTPRoute to be accepted...")
		Expect(k8sClient.WaitForHTTPRouteReady(ctx, TestNamespace, HTTPRouteName, DeploymentTimeout)).To(Succeed())
		defaultLogger.Log("[IntegrationTest] HTTPRoute is ready")

		// Get pod name for cluster checking
		// We need to find a pod to check the cluster configuration
		labelSelector := fmt.Sprintf("%s=%s,%s=%s", EnvoyProxyOwningGatewayLabelKey, GatewayName, EnvoyProxyComponentLabelKey, EnvoyProxyComponentLabelValue)
		pods, err := k8sClient.GetClientset().CoreV1().Pods(EnvoyGatewayNamespace).List(ctx, metav1.ListOptions{
			LabelSelector: labelSelector,
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(len(pods.Items)).To(BeNumerically(">", 0), "No pods found for EnvoyProxy")
		podNameForClusterCheck := pods.Items[0].Name

		// Wait for the Envoy pod to be ready before accessing admin API
		By("Waiting for Envoy pod to be ready")
		defaultLogger.Logf("[IntegrationTest] Waiting for Envoy pod %s to be ready...", podNameForClusterCheck)
		podReadyCtx, podReadyCancel := context.WithTimeout(ctx, DeploymentTimeout)
		defer podReadyCancel()
		err = wait.PollUntilContextTimeout(podReadyCtx, TestPollInterval, DeploymentTimeout, true, func(ctx context.Context) (bool, error) {
			pod, err := k8sClient.GetClientset().CoreV1().Pods(EnvoyGatewayNamespace).Get(ctx, podNameForClusterCheck, metav1.GetOptions{})
			if err != nil {
				return false, err
			}
			for _, condition := range pod.Status.Conditions {
				if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
					defaultLogger.Logf("[IntegrationTest] Envoy pod %s is ready", podNameForClusterCheck)
					return true, nil
				}
			}
			return false, nil
		})
		Expect(err).NotTo(HaveOccurred(), "Envoy pod should be ready")

		// Give Envoy a moment to fully start and open the admin API port
		defaultLogger.Log("[IntegrationTest] Waiting a moment for Envoy admin API to be ready...")
		time.Sleep(5 * time.Second)

		// Wait for the cluster to be created
		// The extension server needs time to be called and modify the cluster configuration
		By("Waiting for backend cluster to be created")
		defaultLogger.Log("[IntegrationTest] Waiting for backend cluster to be created...")
		clusterTimeoutCtx, clusterCancel := context.WithTimeout(ctx, DeploymentTimeout)
		defer clusterCancel()
		err = wait.PollUntilContextTimeout(clusterTimeoutCtx, TestPollInterval, DeploymentTimeout, true, func(ctx context.Context) (bool, error) {
			// Use a shorter timeout for each individual call to avoid blocking the entire polling loop
			callCtx, callCancel := context.WithTimeout(ctx, 30*time.Second)
			defer callCancel()

			adminConfigDump, err := getEnvoyAdminConfigDump(callCtx, k8sClient, EnvoyGatewayNamespace, podNameForClusterCheck)
			if err != nil {
				defaultLogger.Logf("[IntegrationTest] Failed to get config dump (will retry): %v", err)
				return false, nil // Continue polling
			}

			// Check if the cluster exists in the config dump - try multiple search patterns
			clusterFound := false
			searchPatterns := []string{
				fmt.Sprintf("name: %s", ExpectedClusterName),
				fmt.Sprintf("\"name\": \"%s\"", ExpectedClusterName),
				fmt.Sprintf("'name': '%s'", ExpectedClusterName),
				fmt.Sprintf("Cluster: %s", ExpectedClusterName),
				fmt.Sprintf("\"%s\"", ExpectedClusterName),
			}

			for _, pattern := range searchPatterns {
				if strings.Contains(adminConfigDump, pattern) {
					clusterFound = true
					break
				}
			}

			// Also check for any cluster containing "httproute" or "test-route" in case the name is slightly different
			if !clusterFound {
				if strings.Contains(adminConfigDump, "httproute") || strings.Contains(adminConfigDump, "test-route") {
					defaultLogger.Logf("[IntegrationTest] Found cluster containing 'httproute' or 'test-route', checking exact match...")
					// Check if any of the found clusters match our expected pattern
					if strings.Contains(adminConfigDump, HTTPRouteName) {
						// Log a sample around the match to see what the actual name is
						idx := strings.Index(adminConfigDump, HTTPRouteName)
						start := max(0, idx-200)
						end := min(len(adminConfigDump), idx+200)
						sample := adminConfigDump[start:end]
						defaultLogger.Logf("[IntegrationTest] Found reference to HTTPRoute name, sample context: %s", sample)
					}
				}
			}

			if clusterFound {
				defaultLogger.Logf("[IntegrationTest] Backend cluster %s found!", ExpectedClusterName)
				return true, nil
			}

			// Log actual cluster names found for debugging
			// Look in the dynamic_active_clusters or static_clusters sections
			// We need to be more precise - only look for cluster names that are at the cluster level
			if strings.Contains(adminConfigDump, "dynamic_active_clusters:") || strings.Contains(adminConfigDump, "static_clusters:") {
				lines := strings.Split(adminConfigDump, "\n")
				clusterNames := []string{}
				inClusterSection := false
				inClusterEntry := false
				indentLevel := 0

				for _, line := range lines {
					trimmed := strings.TrimSpace(line)
					originalIndent := len(line) - len(trimmed)

					if strings.Contains(trimmed, "dynamic_active_clusters:") || strings.Contains(trimmed, "static_clusters:") {
						inClusterSection = true
						continue
					}

					if inClusterSection {
						// Check if we're entering a new cluster entry (usually starts with "-" or "cluster:")
						if strings.HasPrefix(trimmed, "-") || strings.Contains(trimmed, "cluster:") {
							inClusterEntry = true
							indentLevel = originalIndent
							continue
						}

						// If we hit another top-level section, stop
						if originalIndent == 0 && trimmed != "" && !strings.HasPrefix(trimmed, "-") {
							if !strings.Contains(strings.ToLower(trimmed), "cluster") {
								break
							}
						}

						// Look for cluster name fields - only at the cluster entry level
						if inClusterEntry && originalIndent > indentLevel {
							if strings.HasPrefix(trimmed, "name:") {
								// Extract the name value
								parts := strings.SplitN(trimmed, ":", 2)
								if len(parts) == 2 {
									name := strings.TrimSpace(strings.Trim(parts[1], "\"'"))
									// Filter out invalid names (headers, variables, etc.)
									if name != "" &&
										!strings.HasPrefix(name, "envoy.") &&
										!strings.HasPrefix(name, "xds_cluster") &&
										!strings.HasPrefix(name, ":") &&
										!strings.HasPrefix(name, "'%") &&
										!strings.HasPrefix(name, "%") &&
										!strings.Contains(name, " ") {
										// Check if we already have this name
										found := false
										for _, existing := range clusterNames {
											if existing == name {
												found = true
												break
											}
										}
										if !found {
											clusterNames = append(clusterNames, name)
										}
									}
								}
							}
						}

						// Reset cluster entry flag if we've moved to a different section
						if originalIndent <= indentLevel && trimmed != "" && !strings.HasPrefix(trimmed, "-") {
							inClusterEntry = false
						}
					}
				}

				if len(clusterNames) > 0 {
					defaultLogger.Logf("[IntegrationTest] Found %d cluster(s) in config dump, looking for: %s", len(clusterNames), ExpectedClusterName)
					// Log all clusters found to help debug
					for _, name := range clusterNames {
						defaultLogger.Logf("[IntegrationTest]  - %s", name)
					}
				} else {
					// If no clusters found, log a sample of the config dump for debugging
					defaultLogger.Logf("[IntegrationTest] No clusters found in expected sections. Config dump sample (first 1000 chars): %s", adminConfigDump[:min(len(adminConfigDump), 1000)])
				}
			}

			return false, nil
		})
		if err != nil {
			// Logs will be collected in AfterEach hook
			defaultLogger.Log("[IntegrationTest] Timeout waiting for backend cluster (logs will be collected in cleanup)")
		}
		Expect(err).NotTo(HaveOccurred(), "Backend cluster should be created")

		// Get Envoy Gateway service
		// Envoy Gateway creates EnvoyProxy resources when a Gateway is created
		// These create Envoy proxy pods that handle HTTP traffic
		// The service name is dynamically generated, so we find it by labels
		By("Getting Envoy Gateway service")
		defaultLogger.Log("[IntegrationTest] Getting Envoy Gateway service...")

		// Find the service using labels that identify it as the Gateway's proxy service
		labelSelector = fmt.Sprintf("%s=%s,%s=%s", EnvoyProxyOwningGatewayLabelKey, GatewayName, EnvoyProxyComponentLabelKey, EnvoyProxyComponentLabelValue)
		services, err := k8sClient.GetClientset().CoreV1().Services(EnvoyGatewayNamespace).List(ctx, metav1.ListOptions{
			LabelSelector: labelSelector,
		})
		Expect(err).NotTo(HaveOccurred(), "Failed to list services in namespace %s", EnvoyGatewayNamespace)
		Expect(len(services.Items)).To(BeNumerically(">", 0), "No service found with selector %s in namespace %s", labelSelector, EnvoyGatewayNamespace)

		svc := &services.Items[0]
		egServiceIP := svc.Spec.ClusterIP
		Expect(egServiceIP).NotTo(BeEmpty())
		defaultLogger.Logf("[IntegrationTest] Envoy Gateway service: %s (ClusterIP: %s)", svc.Name, egServiceIP)

		// Port forward to Envoy Gateway service
		By("Setting up port forward to Envoy Gateway service")
		defaultLogger.Logf("[IntegrationTest] Setting up port forward from localhost:%d to service %s/%s...", EnvoyGatewayHostPort, EnvoyGatewayNamespace, svc.Name)
		stopChan, readyChan, errChan := make(chan struct{}, 1), make(chan struct{}, 1), make(chan error, 1)

		// Port forward directly to the service
		// Note: Kubernetes port-forward to services requires finding a pod behind the service
		// Get a pod from the service using its selector
		Expect(len(svc.Spec.Selector)).To(BeNumerically(">", 0), "Service must have a selector")

		// Build label selector from service selector
		selectorParts := []string{}
		for k, v := range svc.Spec.Selector {
			selectorParts = append(selectorParts, fmt.Sprintf("%s=%s", k, v))
		}
		labelSelector = strings.Join(selectorParts, ",")
		defaultLogger.Logf("[IntegrationTest] Finding pod using service selector: %s", labelSelector)

		pods, err = k8sClient.GetClientset().CoreV1().Pods(EnvoyGatewayNamespace).List(ctx, metav1.ListOptions{
			LabelSelector: labelSelector,
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(len(pods.Items)).To(BeNumerically(">", 0), "No pods found for service %s with selector %s", svc.Name, labelSelector)

		podName := pods.Items[0].Name
		defaultLogger.Logf("[IntegrationTest] Port forwarding to pod: %s (behind service %s)", podName, svc.Name)

		// Find the target port from the service
		// The service port 80 maps to targetPort (typically 10080 for Envoy proxy)
		var targetPort int32 = 10080 // Default Envoy proxy port
		for _, port := range svc.Spec.Ports {
			if port.Name == "http-80" || port.Port == 80 {
				// TargetPort can be IntVal (int32) or StrVal (string)
				if port.TargetPort.IntVal != 0 {
					targetPort = port.TargetPort.IntVal
				} else if port.TargetPort.StrVal != "" {
					// If it's a named port, we'd need to look it up in the pod spec
					// For now, default to 10080 which is the standard Envoy proxy port
					targetPort = 10080
					defaultLogger.Logf("[IntegrationTest] Service port %d has named targetPort %s, using default %d", port.Port, port.TargetPort.StrVal, targetPort)
				}
				defaultLogger.Logf("[IntegrationTest] Service port %d maps to pod port %d", port.Port, targetPort)
				break
			}
		}

		// Create port forwarder using REST config
		restConfig := k8sClient.GetConfig()
		path := fmt.Sprintf("/api/v1/namespaces/%s/pods/%s/portforward", EnvoyGatewayNamespace, podName)

		transport, upgrader, err := spdy.RoundTripperFor(restConfig)
		Expect(err).NotTo(HaveOccurred())

		// Parse the host URL
		baseURL, err := url.Parse(restConfig.Host)
		Expect(err).NotTo(HaveOccurred())
		baseURL.Path = path

		dialer := spdy.NewDialer(upgrader, &http.Client{Transport: transport}, "POST", baseURL)

		// Port forward mapping: localhost:8080 -> pod:targetPort
		portMapping := fmt.Sprintf("%d:%d", EnvoyGatewayHostPort, targetPort)
		defaultLogger.Logf("[IntegrationTest] Port forward mapping: %s", portMapping)
		fw, err := portforward.New(dialer, []string{portMapping}, stopChan, readyChan, nil, nil)
		Expect(err).NotTo(HaveOccurred())

		go func() {
			if err := fw.ForwardPorts(); err != nil {
				errChan <- err
			}
		}()

		defer close(stopChan)

		// Wait for port forward to be ready
		defaultLogger.Log("[IntegrationTest] Waiting for port forward to be ready...")
		// Use context with timeout for port forward wait
		portForwardCtx, portForwardCancel := context.WithTimeout(ctx, WaitTimeout)
		defer portForwardCancel()

		select {
		case <-portForwardCtx.Done():
			Fail(fmt.Sprintf("context cancelled or timeout while waiting for port forward: %v", portForwardCtx.Err()))
		case err := <-errChan:
			Fail(fmt.Sprintf("port forward error: %v", err))
		case <-readyChan:
			defaultLogger.Log("[IntegrationTest] Port forward ready")
		}

		// Debug: Check what ports the pod is listening on
		By("Checking Envoy pod ports")
		defaultLogger.Log("[IntegrationTest] Checking Envoy pod container ports...")
		pod, err := k8sClient.GetClientset().CoreV1().Pods(EnvoyGatewayNamespace).Get(ctx, podName, metav1.GetOptions{})
		if err == nil {
			for _, container := range pod.Spec.Containers {
				defaultLogger.Logf("[IntegrationTest] Container %s ports:", container.Name)
				for _, port := range container.Ports {
					defaultLogger.Logf("[IntegrationTest]   - %s: %d/%s (hostPort: %d)", port.Name, port.ContainerPort, port.Protocol, port.HostPort)
				}
			}
		}

		// Debug: Check EnvoyProxy resource status
		By("Checking EnvoyProxy resource")
		defaultLogger.Log("[IntegrationTest] Checking EnvoyProxy resource status...")
		envoyProxyGVRForDebug := schema.GroupVersionResource{
			Group:    "gateway.envoyproxy.io",
			Version:  "v1alpha1",
			Resource: "envoyproxies",
		}
		envoyProxyClientForDebug := k8sClient.GetDynamicClient().Resource(envoyProxyGVRForDebug).Namespace(EnvoyGatewayNamespace)
		envoyProxyList, err := envoyProxyClientForDebug.List(ctx, metav1.ListOptions{
			LabelSelector: fmt.Sprintf("%s=%s", EnvoyProxyOwningGatewayLabelKey, GatewayName),
		})
		if err == nil {
			defaultLogger.Logf("[IntegrationTest] Found %d EnvoyProxy resource(s)", len(envoyProxyList.Items))
			for _, ep := range envoyProxyList.Items {
				defaultLogger.Logf("[IntegrationTest] EnvoyProxy: %s", ep.GetName())
				if status, ok := ep.Object["status"].(map[string]interface{}); ok {
					defaultLogger.Logf("[IntegrationTest]   Status: %+v", status)
				}
			}
		} else {
			defaultLogger.Logf("[IntegrationTest] Warning: Failed to list EnvoyProxy resources: %v", err)
		}

		// Send HTTP request through Envoy Gateway
		By("Sending HTTP request through Envoy Gateway")
		defaultLogger.Logf("[IntegrationTest] Sending HTTP GET request to http://localhost:%d/...", EnvoyGatewayHostPort)
		client := &http.Client{
			Timeout: HTTPClientTimeout,
		}
		resp, err := client.Get(fmt.Sprintf("http://localhost:%d/", EnvoyGatewayHostPort))
		if err != nil {
			defaultLogger.Logf("[IntegrationTest] HTTP request failed: %v", err)
		}
		// Logs will be collected in AfterEach cleanup

		Expect(err).NotTo(HaveOccurred())
		if resp != nil {
			defer resp.Body.Close()
		}

		// Verify response
		defaultLogger.Logf("[IntegrationTest] Received HTTP response: StatusCode=%d", resp.StatusCode)
		Expect(resp.StatusCode).To(Equal(http.StatusOK))
		Expect(resp.Body).NotTo(BeNil())
		defaultLogger.Log("[IntegrationTest] === Test completed successfully ===")
	})
})

// applyCustomResources applies custom resource YAML using dynamic client
func applyCustomResources(ctx context.Context, k8sClient *K8sClient, yamlContent string) error {
	// Split by ---
	manifests := strings.Split(yamlContent, "---")
	for _, manifest := range manifests {
		manifest = strings.TrimSpace(manifest)
		if manifest == "" {
			continue
		}

		// Use the K8sClient's ApplyYAML method which handles unstructured resources
		if err := k8sClient.ApplyYAML(ctx, manifest); err != nil {
			return fmt.Errorf("failed to apply resource: %w", err)
		}
		defaultLogger.Log("[IntegrationTest] Applied resource successfully")
	}

	return nil
}

// ptrOf returns a pointer to the given value
func ptrOf[T any](t T) *T {
	return &t
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
			errChan <- err
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
		// Port forward ready, give it a moment to establish
		time.Sleep(1 * time.Second)
	}

	// Make request to admin API with retries
	adminURL := fmt.Sprintf("http://localhost:%d%s", EnvoyAdminPortForwardPort, adminPath)
	client := &http.Client{
		Timeout: 10 * time.Second,
	}

	var resp *http.Response
	var lastErr error
	// Retry a few times in case Envoy admin API is still starting
	for i := 0; i < 3; i++ {
		resp, lastErr = client.Get(adminURL)
		if lastErr == nil {
			break
		}
		if i < 2 {
			time.Sleep(2 * time.Second)
		}
	}
	if lastErr != nil {
		return "", fmt.Errorf("failed to get config dump after retries: %w", lastErr)
	}
	if resp == nil {
		return "", fmt.Errorf("failed to get config dump: response is nil")
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
		// If we can't create the directory, try using system temp directory
		logsDir = filepath.Join(os.TempDir(), "xds-backend-e2e-logs")
		if err := os.MkdirAll(logsDir, 0755); err != nil {
			return ""
		}
	}

	// Create a timestamped filename
	timestamp := time.Now().Format(LogTimestampFormat)
	filename := fmt.Sprintf("%s-%s.log", logType, timestamp)
	filepath := filepath.Join(logsDir, filename)

	if err := os.WriteFile(filepath, []byte(content), 0644); err != nil {
		return ""
	}

	return filepath
}
