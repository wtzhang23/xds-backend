package e2e

import (
	"context"
	"encoding/base64"
	"flag"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/errors"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var (
	ctx               context.Context
	cancel            context.CancelFunc
	cluster           *KindCluster
	egInstaller       *EnvoyGatewayInstaller
	extensionDeployer *ExtensionServerDeployer
	testService       *TestServiceDeployer
	testService2      *TestServiceDeployer
	envoyGatewayImage string
	runExperimental   bool
	testServiceCACert []byte // CA certificate used for test service TLS (shared with filexds config)
)

func init() {
	flag.StringVar(&envoyGatewayImage, "envoy-gateway-image", "", "Envoy Gateway Docker image (e.g., envoyproxy/gateway:v1.6.0 or envoyproxy/gateway:latest). If empty, uses the default from the Helm chart.")
	flag.BoolVar(&runExperimental, "experimental", false, "Run experimental tests (e.g., BackendTLSPolicy test)")
}

func TestE2E(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "E2E Suite")
}

var _ = BeforeSuite(func() {
	defaultLogger.Log("=== E2E Test Suite Starting ===")

	if os.Getenv("SKIP_E2E") == "true" {
		defaultLogger.Log("SKIP_E2E environment variable is set, skipping e2e tests")
		Skip("Skipping e2e tests")
	}

	ctx, cancel = context.WithTimeout(context.Background(), TestTimeout)

	setupKindCluster(ctx)
	preloadImages(ctx)

	// Create k8sClient
	kubeconfigPath := cluster.GetKubeconfigPath()
	k8sClient, err := NewK8sClient(kubeconfigPath)
	Expect(err).NotTo(HaveOccurred())

	setupTestService(ctx, k8sClient)

	// Deploy extension server
	By("Deploying extension server")
	defaultLogger.Logf("Deploying extension server in namespace: %s", ExtensionServerNamespace)
	Expect(k8sClient.CreateNamespace(ctx, ExtensionServerNamespace)).To(Succeed())

	extensionDeployer, err = NewExtensionServerDeployer(kubeconfigPath)
	Expect(err).NotTo(HaveOccurred())
	defaultLogger.Log("✓ Extension server deployer created")

	tlsSecretName := "xds-backend-tls"
	extensionServerFQDN := fmt.Sprintf("%s.%s.svc.cluster.local", ExtensionServerReleaseName, ExtensionServerNamespace)
	Expect(CreateTLSSecret(ctx, k8sClient, ExtensionServerNamespace, tlsSecretName, extensionServerFQDN)).To(Succeed())

	extensionDeployer.SetCluster(cluster)
	Expect(extensionDeployer.DeployWithTLS(ctx, ExtensionServerNamespace, true, true, ExtensionServerTLSPort, tlsSecretName)).To(Succeed())
	defaultLogger.Log("✓ Extension server deployed")

	defaultLogger.Log("Waiting for extension server pods to be ready...")
	Expect(extensionDeployer.WaitForReady(ctx, ExtensionServerNamespace)).To(Succeed())
	defaultLogger.Log("✓ Extension server is ready")

	setupEnvoyGateway(ctx, k8sClient)
	waitForCRDs(ctx, k8sClient)

	// Setup additional test service (before creating configmap)
	setupAdditionalTestService(ctx, k8sClient)

	setupFilexdsConfigMaps(ctx, k8sClient)
	setupGatewayResources(ctx, k8sClient)
	setupFilexdsServer(ctx, k8sClient)

	defaultLogger.Log("=== E2E Test Suite Setup Complete ===")
})

// setupKindCluster creates and waits for the kind cluster to be ready
func setupKindCluster(ctx context.Context) {
	By("Setting up kind cluster")
	defaultLogger.Logf("Creating kind cluster: %s", ClusterName)
	var err error
	cluster, err = NewKindCluster(ClusterName)
	Expect(err).NotTo(HaveOccurred())
	Expect(cluster.Create(ctx)).To(Succeed())
	defaultLogger.Logf("✓ Kind cluster created: %s", ClusterName)

	defaultLogger.Log("Waiting for kind cluster to be ready...")
	Expect(cluster.WaitForReady(ctx)).To(Succeed())
	defaultLogger.Log("✓ Kind cluster is ready")
}

// preloadImages preloads Docker images into the kind cluster
func preloadImages(ctx context.Context) {
	By("Preloading Docker images into kind cluster")
	extensionImage := fmt.Sprintf("%s:%s", ExtensionServerImageRepo, ExtensionServerImageTag)
	defaultLogger.Logf("Preloading extension server image into kind: %s", extensionImage)
	Expect(cluster.LoadImage(ctx, extensionImage)).To(Succeed())

	if strings.TrimSpace(envoyGatewayImage) != "" {
		defaultLogger.Logf("Preloading Envoy Gateway image into kind: %s", envoyGatewayImage)
		Expect(cluster.LoadImage(ctx, envoyGatewayImage)).To(Succeed())
	}
}

// setupTestService deploys the main test HTTP service
func setupTestService(ctx context.Context, k8sClient *K8sClient) {
	kubeconfigPath := cluster.GetKubeconfigPath()
	defaultLogger.Logf("Kubeconfig: %s", kubeconfigPath)
	defaultLogger.Logf("Deploying test HTTP service %s in namespace %s...", TestServiceName, TestNamespace)

	var err error
	testService, err = NewTestServiceDeployer(kubeconfigPath)
	Expect(err).NotTo(HaveOccurred())

	Expect(k8sClient.CreateNamespace(ctx, TestNamespace)).To(Succeed())

	backendFQDN := fmt.Sprintf("%s.%s.svc.cluster.local", TestServiceName, TestNamespace)
	caCertPEM, serverCertPEM, serverKeyPEM, err := GenerateSelfSignedCert(backendFQDN)
	Expect(err).NotTo(HaveOccurred())
	// Store CA cert for use in filexds configmap
	testServiceCACert = caCertPEM

	err = k8sClient.DeleteSecret(ctx, TestNamespace, TestServiceTLSSecretName)
	if err != nil && !errors.IsNotFound(err) {
		Expect(err).NotTo(HaveOccurred(), "Failed to delete existing TLS secret")
	}

	certBase64 := base64.StdEncoding.EncodeToString(serverCertPEM)
	keyBase64 := base64.StdEncoding.EncodeToString(serverKeyPEM)

	Expect(applyTemplate(ctx, k8sClient, "test-service-tls-secret.yaml", TestServiceTLSSecretTemplate{
		TestServiceTLSSecretName: TestServiceTLSSecretName,
		TestNamespace:            TestNamespace,
		TestServiceTLSCertBase64: certBase64,
		TestServiceTLSKeyBase64:  keyBase64,
	})).To(Succeed())

	Expect(testService.DeployWithTLS(ctx, TestNamespace, TestServiceName, TestServicePort, TestServiceTLSSecretName, TestServiceTLSPort)).To(Succeed())
	defaultLogger.Log("Test service deployed, waiting for ready...")
	Expect(testService.WaitForReady(ctx, TestNamespace, TestServiceName)).To(Succeed())
	defaultLogger.Log("✓ Test service is ready")
}

// setupEnvoyGateway installs and waits for Envoy Gateway to be ready
func setupEnvoyGateway(ctx context.Context, k8sClient *K8sClient) {
	By("Installing Envoy Gateway")
	defaultLogger.Logf("Installing Envoy Gateway in namespace: %s", EnvoyGatewayNamespace)
	var err error
	egInstaller, err = NewEnvoyGatewayInstaller(cluster.GetKubeconfigPath())
	Expect(err).NotTo(HaveOccurred())
	defaultLogger.Log("✓ Envoy Gateway installer created")

	extensionServerFQDN := fmt.Sprintf("%s.%s.svc.cluster.local", ExtensionServerReleaseName, ExtensionServerNamespace)
	defaultLogger.Logf("Configuring Envoy Gateway with extension server at %s:%d", extensionServerFQDN, ExtensionServerPort)

	Expect(egInstaller.Install(ctx, extensionServerFQDN, ExtensionServerPort, envoyGatewayImage)).To(Succeed())
	defaultLogger.Log("✓ Envoy Gateway Helm chart installed")

	defaultLogger.Log("Waiting for Envoy Gateway pods to be ready...")
	Expect(egInstaller.WaitForReady(ctx, EnvoyGatewayNamespace)).To(Succeed())
	defaultLogger.Log("✓ Envoy Gateway is ready")
}

// waitForCRDs waits for all required CRDs to be available
func waitForCRDs(ctx context.Context, k8sClient *K8sClient) {
	defaultLogger.Log("Waiting for CRDs to be available...")
	Expect(k8sClient.WaitForCRD(ctx, "gateways.gateway.networking.k8s.io")).To(Succeed())
	Expect(k8sClient.WaitForCRD(ctx, "httproutes.gateway.networking.k8s.io")).To(Succeed())
	Expect(k8sClient.WaitForCRD(ctx, "gatewayclasses.gateway.networking.k8s.io")).To(Succeed())
	Expect(k8sClient.WaitForCRD(ctx, "xdsbackends.xdsbackend.wtzhang23.github.io")).To(Succeed())
	Expect(k8sClient.WaitForCRD(ctx, "envoyproxies.gateway.envoyproxy.io")).To(Succeed())
	Expect(k8sClient.WaitForCRD(ctx, "backendtlspolicies.gateway.networking.k8s.io")).To(Succeed())

	// Refresh REST mapper to discover newly installed CRDs
	Expect(k8sClient.RefreshRESTMapper(ctx)).To(Succeed())
	time.Sleep(2 * time.Second) // Give mapper time to refresh

	defaultLogger.Log("✓ Common CRDs are available")
}

// setupFilexdsConfigMaps creates the filexds server and Envoy ConfigMaps
func setupFilexdsConfigMaps(ctx context.Context, k8sClient *K8sClient) {
	defaultLogger.Log("Creating filexds server and Envoy ConfigMaps...")
	testServiceIP, err := testService.GetServiceClusterIP(ctx, TestNamespace, TestServiceName)
	Expect(err).NotTo(HaveOccurred())

	// Use the same CA cert that was used to sign the test service certificate
	if testServiceCACert == nil {
		Fail("testServiceCACert not set - setupTestService must be called before setupFilexdsConfigMaps")
	}
	caCertPEM := string(testServiceCACert)

	// Build services list
	services := []ServiceEndpoint{
		{
			ServiceName: TestServiceName,
			ServiceIP:   testServiceIP,
			ServicePort: TestServicePort,
		},
		{
			ServiceName: TestServiceName + "-tls",
			ServiceIP:   testServiceIP,
			ServicePort: TestServiceTLSPort,
		},
	}

	// Add service-2 (always deployed)
	if testService2 != nil {
		service2IP, err := testService2.GetServiceClusterIP(ctx, TestNamespace, TestService2Name)
		Expect(err).NotTo(HaveOccurred())
		services = append(services, ServiceEndpoint{
			ServiceName: TestService2Name,
			ServiceIP:   service2IP,
			ServicePort: TestService2Port,
		})
	}

	// Use the unified template that supports multiple services
	Expect(applyTemplate(ctx, k8sClient, "filexds-server-configmap.yaml", FileXdsServerConfigMapMultipleBackendsTemplate{
		FileXdsServerConfigMapName: FileXdsServerConfigMapName,
		EnvoyGatewayNamespace:      EnvoyGatewayNamespace,
		Services:                   services,
		TlsCaCertPEM:               caCertPEM,
		TlsCaCertName:              InlineTLSCACertResourceName,
	})).To(Succeed())

	Expect(applyTemplate(ctx, k8sClient, "envoy-eds-configmap.yaml", EnvoyEdsConfigMapTemplate{
		EnvoyEdsConfigMapName: EnvoyEdsConfigMapName,
		EnvoyGatewayNamespace: EnvoyGatewayNamespace,
		TestServiceName:       TestServiceName,
		TestServiceIP:         testServiceIP,
		TestServicePort:       TestServicePort,
	})).To(Succeed())
	defaultLogger.Log("✓ Filexds server and Envoy ConfigMaps created")
}

// setupGatewayResources creates EnvoyProxy, GatewayClass, and Gateway resources
func setupGatewayResources(ctx context.Context, k8sClient *K8sClient) {
	defaultLogger.Log("Creating EnvoyProxy...")
	fileXdsServiceFQDN := fmt.Sprintf("%s.%s.svc.cluster.local", FileXdsServiceName, EnvoyGatewayNamespace)
	err := applyTemplate(ctx, k8sClient, "envoyproxy.yaml", EnvoyProxyTemplate{
		GatewayClassName:      GatewayClassName,
		EnvoyGatewayNamespace: EnvoyGatewayNamespace,
		EnvoyEdsConfigMapName: EnvoyEdsConfigMapName,
		FileXdsClusterName:    FileXdsClusterName,
		FileXdsServiceFQDN:    fileXdsServiceFQDN,
		FileXdsPort:           FileXdsPort,
	})
	if err != nil {
		defaultLogger.Logf("Warning: Failed to create EnvoyProxy (may already exist): %v", err)
	} else {
		time.Sleep(EnvoyProxyProcessingDelay)
	}

	defaultLogger.Log("Creating/updating GatewayClass...")
	Expect(applyTemplate(ctx, k8sClient, "gatewayclass.yaml", GatewayClassTemplate{
		GatewayClassName:      GatewayClassName,
		EnvoyGatewayNamespace: EnvoyGatewayNamespace,
	})).To(Succeed())
	defaultLogger.Log("✓ EnvoyProxy and GatewayClass configured")

	defaultLogger.Log("Creating Gateway...")
	Expect(applyTemplate(ctx, k8sClient, "gateway.yaml", GatewayTemplate{
		GatewayName:           GatewayName,
		EnvoyGatewayNamespace: EnvoyGatewayNamespace,
		GatewayClassName:      GatewayClassName,
		GatewayListenerName:   GatewayListenerName,
		GatewayListenerPort:   GatewayListenerPort,
	})).To(Succeed())
	Expect(k8sClient.WaitForGatewayReady(ctx, EnvoyGatewayNamespace, GatewayName, DeploymentTimeout)).To(Succeed())
	defaultLogger.Log("✓ Gateway created and ready")
}

// setupFilexdsServer deploys the filexds server
func setupFilexdsServer(ctx context.Context, k8sClient *K8sClient) {
	defaultLogger.Log("Deploying filexds server...")
	image := fmt.Sprintf("%s:%s", ExtensionServerImageRepo, ExtensionServerImageTag)
	Expect(cluster.LoadImage(ctx, image)).To(Succeed())

	Expect(applyTemplate(ctx, k8sClient, "filexds-deployment.yaml", FileXdsDeploymentTemplate{
		FileXdsDeploymentName:      FileXdsDeploymentName,
		EnvoyGatewayNamespace:      EnvoyGatewayNamespace,
		ExtensionServerImageRepo:   ExtensionServerImageRepo,
		ExtensionServerImageTag:    ExtensionServerImageTag,
		ImagePullPolicy:            ImagePullPolicy,
		FileXdsPort:                FileXdsPort,
		FileXdsConfigPath:          FileXdsConfigPath,
		FileXdsConfigDir:           FileXdsConfigDir,
		FileXdsServerConfigMapName: FileXdsServerConfigMapName,
	})).To(Succeed())

	Expect(applyTemplate(ctx, k8sClient, "filexds-service.yaml", FileXdsServiceTemplate{
		FileXdsServiceName:    FileXdsServiceName,
		FileXdsDeploymentName: FileXdsDeploymentName,
		EnvoyGatewayNamespace: EnvoyGatewayNamespace,
		FileXdsPort:           FileXdsPort,
	})).To(Succeed())

	Expect(k8sClient.WaitForPodsReady(ctx, EnvoyGatewayNamespace, fmt.Sprintf("app=%s", FileXdsDeploymentName), DeploymentTimeout)).To(Succeed())
	defaultLogger.Log("✓ Filexds server deployed and ready")
}

// setupAdditionalTestService deploys service-2
func setupAdditionalTestService(ctx context.Context, k8sClient *K8sClient) {
	defaultLogger.Log("Setting up additional test service...")

	var err error
	testService2, err = NewTestServiceDeployer(cluster.GetKubeconfigPath())
	Expect(err).NotTo(HaveOccurred())

	// Use DeployWithTLS with empty TLS secret to deploy without TLS
	Expect(testService2.DeployWithTLS(ctx, TestNamespace, TestService2Name, TestService2Port, "", 0)).To(Succeed())
	Expect(testService2.WaitForReady(ctx, TestNamespace, TestService2Name)).To(Succeed())
	defaultLogger.Logf("✓ Test service %s is ready", TestService2Name)
}

var _ = AfterSuite(func() {
	defaultLogger.Log("\n=== E2E Test Suite Cleanup ===")

	if cancel != nil {
		cancel()
	}

	if cluster != nil {
		By("Cleaning up kind cluster")
		defaultLogger.Logf("Deleting kind cluster: %s", ClusterName)
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), CleanupTimeout)
		defer cleanupCancel()
		if err := cluster.Delete(cleanupCtx); err != nil {
			defaultLogger.Logf("Warning: Failed to delete cluster: %v", err)
		} else {
			defaultLogger.Logf("✓ Kind cluster deleted: %s", ClusterName)
		}
	}

	defaultLogger.Log("=== E2E Test Suite Cleanup Complete ===")
})
