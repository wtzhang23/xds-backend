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
	envoyGatewayImage string
)

func init() {
	flag.StringVar(&envoyGatewayImage, "envoy-gateway-image", "", "Envoy Gateway Docker image (e.g., envoyproxy/gateway:v1.6.0 or envoyproxy/gateway:latest). If empty, uses the default from the Helm chart.")
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

	// Preload images into kind if needed
	By("Preloading Docker images into kind cluster")
	// Always preload the extension server image (typically a local build)
	extensionImage := fmt.Sprintf("%s:%s", ExtensionServerImageRepo, ExtensionServerImageTag)
	defaultLogger.Logf("Preloading extension server image into kind: %s", extensionImage)
	Expect(cluster.LoadImage(ctx, extensionImage)).To(Succeed())

	// Optionally preload a custom Envoy Gateway image if provided
	if strings.TrimSpace(envoyGatewayImage) != "" {
		defaultLogger.Logf("Preloading Envoy Gateway image into kind: %s", envoyGatewayImage)
		Expect(cluster.LoadImage(ctx, envoyGatewayImage)).To(Succeed())
	}

	kubeconfigPath := cluster.GetKubeconfigPath()
	defaultLogger.Logf("Kubeconfig: %s", kubeconfigPath)
	defaultLogger.Logf("Deploying test HTTP service %s in namespace %s...", TestServiceName, TestNamespace)
	testService, err = NewTestServiceDeployer(cluster.GetKubeconfigPath())
	Expect(err).NotTo(HaveOccurred())

	k8sClient, err := NewK8sClient(cluster.GetKubeconfigPath())
	Expect(err).NotTo(HaveOccurred())
	Expect(k8sClient.CreateNamespace(ctx, TestNamespace)).To(Succeed())

	backendFQDN := fmt.Sprintf("%s.%s.svc.cluster.local", TestServiceName, TestNamespace)
	backendCertPEM, backendKeyPEM, err := GenerateSelfSignedCert(backendFQDN)
	Expect(err).NotTo(HaveOccurred())

	// Delete existing secret to avoid conflicts during re-runs
	err = k8sClient.DeleteSecret(ctx, TestNamespace, TestServiceTLSSecretName)
	if err != nil && !errors.IsNotFound(err) {
		Expect(err).NotTo(HaveOccurred(), "Failed to delete existing TLS secret")
	}

	certBase64 := base64.StdEncoding.EncodeToString(backendCertPEM)
	keyBase64 := base64.StdEncoding.EncodeToString(backendKeyPEM)

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

	By("Deploying extension server")
	defaultLogger.Logf("Deploying extension server in namespace: %s", ExtensionServerNamespace)
	Expect(k8sClient.CreateNamespace(ctx, ExtensionServerNamespace)).To(Succeed())

	extensionDeployer, err = NewExtensionServerDeployer(cluster.GetKubeconfigPath())
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

	By("Installing Envoy Gateway")
	defaultLogger.Logf("Installing Envoy Gateway in namespace: %s", EnvoyGatewayNamespace)
	egInstaller, err = NewEnvoyGatewayInstaller(cluster.GetKubeconfigPath())
	Expect(err).NotTo(HaveOccurred())
	defaultLogger.Log("✓ Envoy Gateway installer created")

	defaultLogger.Logf("Configuring Envoy Gateway with extension server at %s:%d", extensionServerFQDN, ExtensionServerPort)

	Expect(egInstaller.Install(ctx, extensionServerFQDN, ExtensionServerPort, envoyGatewayImage)).To(Succeed())
	defaultLogger.Log("✓ Envoy Gateway Helm chart installed")

	defaultLogger.Log("Waiting for Envoy Gateway pods to be ready...")
	Expect(egInstaller.WaitForReady(ctx, EnvoyGatewayNamespace)).To(Succeed())
	defaultLogger.Log("✓ Envoy Gateway is ready")

	defaultLogger.Log("Waiting for CRDs to be available...")
	k8sClient, err = NewK8sClient(cluster.GetKubeconfigPath())
	Expect(err).NotTo(HaveOccurred())
	Expect(k8sClient.WaitForCRD(ctx, "gateways.gateway.networking.k8s.io")).To(Succeed())
	Expect(k8sClient.WaitForCRD(ctx, "httproutes.gateway.networking.k8s.io")).To(Succeed())
	Expect(k8sClient.WaitForCRD(ctx, "gatewayclasses.gateway.networking.k8s.io")).To(Succeed())
	Expect(k8sClient.WaitForCRD(ctx, "xdsbackends.xdsbackend.wtzhang23.github.io")).To(Succeed())
	Expect(k8sClient.WaitForCRD(ctx, "envoyproxies.gateway.envoyproxy.io")).To(Succeed())
	Expect(k8sClient.WaitForCRD(ctx, "backendtlspolicies.gateway.networking.k8s.io")).To(Succeed())
	defaultLogger.Log("✓ Common CRDs are available")

	defaultLogger.Log("Creating filexds server and Envoy ConfigMaps...")
	testServiceIP, err := testService.GetServiceClusterIP(ctx, TestNamespace, TestServiceName)
	Expect(err).NotTo(HaveOccurred())

	// Get the TLS certificate from the test service secret to include in the ConfigMaps
	// Note: backendCertPEM is already available from earlier in BeforeSuite
	caCertPEM := string(backendCertPEM)

	// Create filexds server ConfigMap (for the filexds server to serve xDS resources)
	Expect(applyTemplate(ctx, k8sClient, "filexds-server-configmap.yaml", FileXdsServerConfigMapTemplate{
		FileXdsServerConfigMapName: FileXdsServerConfigMapName,
		EnvoyGatewayNamespace:      EnvoyGatewayNamespace,
		TestServiceName:            TestServiceName,
		TestServiceIP:              testServiceIP,
		TestServicePort:            TestServicePort,
		TestServiceTLSPort:         TestServiceTLSPort,
		TlsCaCertPEM:               caCertPEM,
		TlsCaCertName:              InlineTLSCACertResourceName,
	})).To(Succeed())

	// Create Envoy EDS ConfigMap (for Envoy's file-based EDS)
	Expect(applyTemplate(ctx, k8sClient, "envoy-eds-configmap.yaml", EnvoyEdsConfigMapTemplate{
		EnvoyEdsConfigMapName: EnvoyEdsConfigMapName,
		EnvoyGatewayNamespace: EnvoyGatewayNamespace,
		TestServiceName:       TestServiceName,
		TestServiceIP:         testServiceIP,
		TestServicePort:       TestServicePort,
	})).To(Succeed())
	defaultLogger.Log("✓ Filexds server and Envoy ConfigMaps created")

	defaultLogger.Log("Creating EnvoyProxy...")
	fileXdsServiceFQDN := fmt.Sprintf("%s.%s.svc.cluster.local", FileXdsServiceName, EnvoyGatewayNamespace)
	err = applyTemplate(ctx, k8sClient, "envoyproxy.yaml", EnvoyProxyTemplate{
		GatewayClassName:       GatewayClassName,
		EnvoyGatewayNamespace:  EnvoyGatewayNamespace,
		EnvoyEdsConfigMapName:  EnvoyEdsConfigMapName,
		FileXdsClusterName:     FileXdsClusterName,
		FileXdsServiceFQDN:     fileXdsServiceFQDN,
		FileXdsPort:            FileXdsPort,
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

	defaultLogger.Log("Deploying filexds server...")
	// testServiceIP already obtained above when creating the ConfigMap

	image := fmt.Sprintf("%s:%s", ExtensionServerImageRepo, ExtensionServerImageTag)
	Expect(cluster.LoadImage(ctx, image)).To(Succeed())

	Expect(applyTemplate(ctx, k8sClient, "filexds-deployment.yaml", FileXdsDeploymentTemplate{
		FileXdsDeploymentName:     FileXdsDeploymentName,
		EnvoyGatewayNamespace:     EnvoyGatewayNamespace,
		ExtensionServerImageRepo:  ExtensionServerImageRepo,
		ExtensionServerImageTag:   ExtensionServerImageTag,
		ImagePullPolicy:           ImagePullPolicy,
		FileXdsPort:               FileXdsPort,
		FileXdsConfigPath:         FileXdsConfigPath,
		FileXdsConfigDir:          FileXdsConfigDir,
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

	defaultLogger.Log("=== E2E Test Suite Setup Complete ===")
})

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
