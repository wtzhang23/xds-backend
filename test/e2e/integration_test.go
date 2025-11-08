package e2e

import (
	"context"
	"fmt"
	"net/http"
	"os/exec"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("xDS Backend Integration", func() {
	var (
		xdsServer         *XdsServer
		testService       *TestServiceDeployer
		extensionDeployer *ExtensionServerDeployer
		xdsServiceExposer *XdsServiceExposer
		xdsServerPort     = 18000
		testServiceName   = "test-http-service"
		testServicePort   = 8080
		testNamespace     = "test-namespace"
		xdsServerName     = "test-xds-server"
		xdsServiceName    = "xds-server"
	)

	BeforeEach(func() {
		var err error
		extensionDeployer, err = NewExtensionServerDeployer(cluster.GetKubeconfigPath())
		Expect(err).NotTo(HaveOccurred())
		extensionDeployer.SetCluster(cluster)
		// Start xDS server
		By("Starting xDS server")
		xdsServer = NewXdsServer(xdsServerPort)
		Expect(xdsServer.Start(ctx)).To(Succeed())

		// Deploy test HTTP service
		By("Deploying test HTTP service")
		testService, err = NewTestServiceDeployer(cluster.GetKubeconfigPath())
		Expect(err).NotTo(HaveOccurred())
		Expect(testService.Deploy(ctx, testNamespace, testServiceName, testServicePort)).To(Succeed())
		Expect(testService.WaitForReady(ctx, testNamespace, testServiceName)).To(Succeed())

		// Get the service endpoint
		By("Getting test service endpoint")
		serviceIP, err := testService.GetServiceClusterIP(ctx, testNamespace, testServiceName)
		Expect(err).NotTo(HaveOccurred())
		Expect(serviceIP).NotTo(BeEmpty())

		// Configure xDS server with EDS endpoints pointing to the test service
		By("Configuring xDS server with EDS endpoints")
		endpoints := []Endpoint{
			{
				Address: serviceIP,
				Port:    testServicePort,
			},
		}
		Expect(xdsServer.SetEndpoints(testServiceName, endpoints)).To(Succeed())

		// Expose xDS server to the cluster via a Kubernetes service
		By("Exposing xDS server to cluster")
		xdsServiceExposer, err = NewXdsServiceExposer(cluster.GetKubeconfigPath(), testNamespace, xdsServiceName, xdsServerPort)
		Expect(err).NotTo(HaveOccurred())
		xdsServiceAddress, err := xdsServiceExposer.Expose(ctx, xdsServer.GetAddress())
		Expect(err).NotTo(HaveOccurred())

		// Update extension server config to include xDS server
		By("Updating extension server configuration")
		Expect(updateExtensionServerConfig(ctx, cluster.GetKubeconfigPath(), xdsServerName, xdsServiceAddress, xdsServerPort)).To(Succeed())

		// Restart extension server to pick up new config
		By("Restarting extension server")
		Expect(restartExtensionServer(ctx, cluster.GetKubeconfigPath(), ExtensionServerNamespace)).To(Succeed())
		Expect(extensionDeployer.WaitForReady(ctx, ExtensionServerNamespace)).To(Succeed())
	})

	AfterEach(func() {
		if xdsServiceExposer != nil {
			_ = xdsServiceExposer.Cleanup(ctx)
		}
		if xdsServer != nil {
			xdsServer.Stop()
		}
	})

	It("should route traffic through Envoy Gateway to test service via xDS EDS", func() {
		// Create an EnvoyGateway resource with HTTPRoute using XdsBackend
		By("Creating EnvoyGateway and HTTPRoute resources")
		envoyGatewayYaml := fmt.Sprintf(`apiVersion: gateway.envoyproxy.io/v1alpha1
kind: EnvoyGateway
metadata:
  name: eg
  namespace: %s
spec:
  extensionServer:
    backendRef:
      group: xdsbackend.wtzhang23.io
      kind: XdsBackend
      name: test-backend
---
apiVersion: gateway.envoyproxy.io/v1alpha1
kind: XdsBackend
metadata:
  name: test-backend
  namespace: %s
spec:
  server: %s
  service: %s
---
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata:
  name: eg
  namespace: %s
spec:
  gatewayClassName: eg
  listeners:
  - name: http
    protocol: HTTP
    port: 80
---
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: test-route
  namespace: %s
spec:
  parentRefs:
  - name: eg
  rules:
  - matches:
    - path:
        type: PathPrefix
        value: /
    backendRefs:
    - group: xdsbackend.wtzhang23.io
      kind: XdsBackend
      name: test-backend
`, EnvoyGatewayNamespace, EnvoyGatewayNamespace, xdsServerName, testServiceName, EnvoyGatewayNamespace, testNamespace)

		// Apply using kubectl for now - could be refactored to use k8s client
		cmd := exec.CommandContext(ctx, "kubectl", "--kubeconfig", cluster.GetKubeconfigPath(),
			"apply", "-f", "-")
		cmd.Stdin = strings.NewReader(envoyGatewayYaml)
		output, err := cmd.CombinedOutput()
		Expect(err).NotTo(HaveOccurred(), "Failed to create EnvoyGateway resources: %s", string(output))

		// Wait for Envoy Gateway to be ready
		By("Waiting for Envoy Gateway to be ready")
		time.Sleep(10 * time.Second) // Give Envoy Gateway time to process

		// Get Envoy Gateway service address
		By("Getting Envoy Gateway service address")
		cmd = exec.CommandContext(ctx, "kubectl", "--kubeconfig", cluster.GetKubeconfigPath(),
			"get", "svc", "-n", EnvoyGatewayNamespace,
			"-l", "app.kubernetes.io/name=envoy-gateway",
			"-o", "jsonpath={.items[0].spec.clusterIP}")
		output, err = cmd.Output()
		Expect(err).NotTo(HaveOccurred())
		egServiceIP := string(output)
		Expect(egServiceIP).NotTo(BeEmpty())

		// Port forward to Envoy Gateway
		By("Setting up port forward to Envoy Gateway")
		portForwardCmd := exec.CommandContext(ctx, "kubectl", "--kubeconfig", cluster.GetKubeconfigPath(),
			"port-forward", "-n", EnvoyGatewayNamespace,
			"svc/envoy-gateway", "8080:80")
		portForwardCmd.Start()
		defer portForwardCmd.Process.Kill()

		// Wait for port forward to be ready
		time.Sleep(2 * time.Second)

		// Send HTTP request through Envoy Gateway
		By("Sending HTTP request through Envoy Gateway")
		client := &http.Client{
			Timeout: 10 * time.Second,
		}
		resp, err := client.Get("http://localhost:8080/")
		Expect(err).NotTo(HaveOccurred())
		defer resp.Body.Close()

		// Verify response
		Expect(resp.StatusCode).To(Equal(http.StatusOK))
		Expect(resp.Body).NotTo(BeNil())
	})
})

// updateExtensionServerConfig updates the extension server configuration to include xDS server
func updateExtensionServerConfig(ctx context.Context, kubeconfigPath, xdsServerName, xdsServerAddress string, xdsServerPort int) error {
	k8sClient, err := NewK8sClient(kubeconfigPath)
	if err != nil {
		return fmt.Errorf("failed to create k8s client: %w", err)
	}
	// Create config with xDS server
	// xdsServerAddress is now a Kubernetes service DNS name (e.g., xds-server.test-namespace.svc.cluster.local)
	configYaml := fmt.Sprintf(`sources:
- name: %s
  cluster:
    name: %s
    connect_timeout: 5s
    type: STRICT_DNS
    load_assignment:
      cluster_name: %s
      endpoints:
      - lb_endpoints:
        - endpoint:
            address:
              socket_address:
                address: %s
                port_value: %d
  config_source:
    resource_api_version: V3
    api_config_source:
      api_type: GRPC
      transport_api_version: V3
      grpc_services:
      - envoy_grpc:
          cluster_name: %s
`, xdsServerName, xdsServerName, xdsServerName, xdsServerAddress, xdsServerPort, xdsServerName)

	// Update ConfigMap
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "xds-backend-config",
			Namespace: ExtensionServerNamespace,
		},
		Data: map[string]string{
			"config.yaml": configYaml,
		},
	}

	if err := k8sClient.CreateConfigMap(ctx, ExtensionServerNamespace, cm); err != nil {
		return fmt.Errorf("failed to update configmap: %w", err)
	}

	return nil
}

// restartExtensionServer restarts the extension server deployment
func restartExtensionServer(ctx context.Context, kubeconfigPath, namespace string) error {
	k8sClient, err := NewK8sClient(kubeconfigPath)
	if err != nil {
		return fmt.Errorf("failed to create k8s client: %w", err)
	}

	if err := k8sClient.RestartDeployment(ctx, namespace, "xds-backend"); err != nil {
		return fmt.Errorf("failed to restart extension server: %w", err)
	}

	return nil
}
