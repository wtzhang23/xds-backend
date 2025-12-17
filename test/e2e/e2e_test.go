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
	testServiceCACert []byte // CA certificate used for test service TLS (set by setupTestServiceTLSSecret)
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
	if os.Getenv("SKIP_E2E") == "true" {
		Skip("Skipping e2e tests")
	}

	ctx, cancel = context.WithTimeout(context.Background(), TestTimeout)

	setupKindCluster(ctx)
	preloadImages(ctx)

	kubeconfigPath := cluster.GetKubeconfigPath()
	k8sClient, err := NewK8sClient(kubeconfigPath)
	Expect(err).NotTo(HaveOccurred())

	setupTestServiceTLSSecret(ctx, k8sClient)
	setupTestService(ctx, k8sClient)

	By("Deploying extension server")
	Expect(k8sClient.CreateNamespace(ctx, ExtensionServerNamespace)).To(Succeed())

	extensionDeployer = NewExtensionServerDeployer(k8sClient)

	tlsSecretName := "xds-backend-tls"
	extensionServerFQDN := fmt.Sprintf("%s.%s.svc.cluster.local", ExtensionServerReleaseName, ExtensionServerNamespace)
	Expect(CreateTLSSecret(ctx, k8sClient, ExtensionServerNamespace, tlsSecretName, extensionServerFQDN)).To(Succeed())

	extensionDeployer.SetCluster(cluster)
	Expect(extensionDeployer.DeployWithTLS(ctx, ExtensionServerNamespace, true, true, ExtensionServerTLSPort, tlsSecretName)).To(Succeed())
	Expect(extensionDeployer.WaitForReady(ctx, ExtensionServerNamespace)).To(Succeed())

	setupEnvoyGateway(ctx, k8sClient)
	waitForCRDs(ctx, k8sClient)

	setupAdditionalTestService(ctx, k8sClient)
	setupFilexdsConfigMaps(ctx, k8sClient)
	setupGatewayResources(ctx, k8sClient)
	setupFilexdsServer(ctx, k8sClient)
})

func setupKindCluster(ctx context.Context) {
	By("Setting up kind cluster")
	defaultLogger.Logf("Creating kind cluster: %s", ClusterName)
	var err error
	cluster, err = NewKindCluster(ClusterName)
	Expect(err).NotTo(HaveOccurred())
	Expect(cluster.Create(ctx)).To(Succeed())
	Expect(cluster.WaitForReady(ctx)).To(Succeed())
	defaultLogger.Logf("Kind cluster %s is ready", ClusterName)
}

func preloadImages(ctx context.Context) {
	By("Preloading Docker images into kind cluster")
	extensionImage := fmt.Sprintf("%s:%s", ExtensionServerImageRepo, ExtensionServerImageTag)
	Expect(cluster.LoadImage(ctx, extensionImage)).To(Succeed())

	if strings.TrimSpace(envoyGatewayImage) != "" {
		Expect(cluster.LoadImage(ctx, envoyGatewayImage)).To(Succeed())
	}
}

func setupTestServiceTLSSecret(ctx context.Context, k8sClient *K8sClient) {
	defaultLogger.Log("Creating test service TLS secret")
	Expect(k8sClient.CreateNamespace(ctx, TestNamespace)).To(Succeed())

	backendFQDN := fmt.Sprintf("%s.%s.svc.cluster.local", TestServiceName, TestNamespace)
	caCertPEM, serverCertPEM, serverKeyPEM, err := GenerateSelfSignedCert(backendFQDN)
	Expect(err).NotTo(HaveOccurred())
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
	defaultLogger.Log("Test service TLS secret created")
}

func setupTestService(ctx context.Context, k8sClient *K8sClient) {
	defaultLogger.Logf("Deploying test service %s", TestServiceName)
	testService = NewTestServiceDeployer(k8sClient)

	Expect(testService.DeployWithTLS(ctx, TestNamespace, TestServiceName, TestServicePort, TestServiceTLSSecretName, TestServiceTLSPort)).To(Succeed())
	Expect(testService.WaitForReady(ctx, TestNamespace, TestServiceName)).To(Succeed())
	defaultLogger.Logf("Test service %s is ready", TestServiceName)
}

func setupEnvoyGateway(ctx context.Context, k8sClient *K8sClient) {
	By("Installing Envoy Gateway")
	defaultLogger.Logf("Installing Envoy Gateway in namespace: %s", EnvoyGatewayNamespace)
	var err error
	egInstaller, err = NewEnvoyGatewayInstaller(k8sClient)
	Expect(err).NotTo(HaveOccurred())

	extensionServerFQDN := fmt.Sprintf("%s.%s.svc.cluster.local", ExtensionServerReleaseName, ExtensionServerNamespace)
	Expect(egInstaller.Install(ctx, extensionServerFQDN, ExtensionServerPort, envoyGatewayImage)).To(Succeed())
	Expect(egInstaller.WaitForReady(ctx, EnvoyGatewayNamespace)).To(Succeed())
	defaultLogger.Log("Envoy Gateway is ready")
}

func waitForCRDs(ctx context.Context, k8sClient *K8sClient) {
	Expect(k8sClient.WaitForCRD(ctx, "gateways.gateway.networking.k8s.io")).To(Succeed())
	Expect(k8sClient.WaitForCRD(ctx, "httproutes.gateway.networking.k8s.io")).To(Succeed())
	Expect(k8sClient.WaitForCRD(ctx, "gatewayclasses.gateway.networking.k8s.io")).To(Succeed())
	Expect(k8sClient.WaitForCRD(ctx, "xdsbackends.xdsbackend.wtzhang23.github.io")).To(Succeed())
	Expect(k8sClient.WaitForCRD(ctx, "envoyproxies.gateway.envoyproxy.io")).To(Succeed())
	Expect(k8sClient.WaitForCRD(ctx, "backendtlspolicies.gateway.networking.k8s.io")).To(Succeed())

	Expect(k8sClient.RefreshRESTMapper(ctx)).To(Succeed())
	time.Sleep(2 * time.Second)
}

func setupFilexdsConfigMaps(ctx context.Context, k8sClient *K8sClient) {
	defaultLogger.Log("Creating filexds server and Envoy ConfigMaps")
	testServiceIP, err := testService.GetServiceClusterIP(ctx, TestNamespace, TestServiceName)
	Expect(err).NotTo(HaveOccurred())

	Expect(testServiceCACert).NotTo(BeNil(), "testServiceCACert not set - setupTestServiceTLSSecret must be called before setupFilexdsConfigMaps")
	caCertPEM := string(testServiceCACert)

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

	if testService2 != nil {
		service2IP, err := testService2.GetServiceClusterIP(ctx, TestNamespace, TestService2Name)
		Expect(err).NotTo(HaveOccurred())
		services = append(services, ServiceEndpoint{
			ServiceName: TestService2Name,
			ServiceIP:   service2IP,
			ServicePort: TestService2Port,
		})
	}

	Expect(applyTemplate(ctx, k8sClient, "filexds-server-configmap.yaml", FileXdsServerConfigMapTemplate{
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
	defaultLogger.Log("Filexds server and Envoy ConfigMaps created")
}

func setupGatewayResources(ctx context.Context, k8sClient *K8sClient) {
	defaultLogger.Log("Creating Gateway resources")
	fileXdsServiceFQDN := fmt.Sprintf("%s.%s.svc.cluster.local", FileXdsServiceName, EnvoyGatewayNamespace)
	err := applyTemplate(ctx, k8sClient, "envoyproxy.yaml", EnvoyProxyTemplate{
		GatewayClassName:      GatewayClassName,
		EnvoyGatewayNamespace: EnvoyGatewayNamespace,
		EnvoyEdsConfigMapName: EnvoyEdsConfigMapName,
		FileXdsClusterName:    FileXdsClusterName,
		FileXdsServiceFQDN:    fileXdsServiceFQDN,
		FileXdsPort:           FileXdsPort,
	})
	if err == nil {
		time.Sleep(EnvoyProxyProcessingDelay)
	}

	Expect(applyTemplate(ctx, k8sClient, "gatewayclass.yaml", GatewayClassTemplate{
		GatewayClassName:      GatewayClassName,
		EnvoyGatewayNamespace: EnvoyGatewayNamespace,
	})).To(Succeed())

	Expect(applyTemplate(ctx, k8sClient, "gateway.yaml", GatewayTemplate{
		GatewayName:           GatewayName,
		EnvoyGatewayNamespace: EnvoyGatewayNamespace,
		GatewayClassName:      GatewayClassName,
		GatewayListenerName:   GatewayListenerName,
		GatewayListenerPort:   GatewayListenerPort,
	})).To(Succeed())
	Expect(k8sClient.WaitForGatewayReady(ctx, EnvoyGatewayNamespace, GatewayName, DeploymentTimeout)).To(Succeed())
	defaultLogger.Log("Gateway resources created")
}

func setupFilexdsServer(ctx context.Context, k8sClient *K8sClient) {
	defaultLogger.Log("Deploying filexds server")
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
	defaultLogger.Log("Filexds server deployed and ready")
}

func setupAdditionalTestService(ctx context.Context, k8sClient *K8sClient) {
	defaultLogger.Logf("Deploying additional test service %s", TestService2Name)
	testService2 = NewTestServiceDeployer(k8sClient)

	Expect(testService2.DeployWithTLS(ctx, TestNamespace, TestService2Name, TestService2Port, "", 0)).To(Succeed())
	Expect(testService2.WaitForReady(ctx, TestNamespace, TestService2Name)).To(Succeed())
	defaultLogger.Logf("Test service %s is ready", TestService2Name)
}

var _ = AfterSuite(func() {
	if cancel != nil {
		cancel()
	}

	if cluster != nil {
		By("Cleaning up kind cluster")
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), CleanupTimeout)
		defer cleanupCancel()
		_ = cluster.Delete(cleanupCtx)
	}
})
