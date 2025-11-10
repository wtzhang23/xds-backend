package e2e

import (
	"context"
	"fmt"
	"os"
	"testing"

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
)

func TestE2E(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "E2E Suite")
}

var _ = BeforeSuite(func() {
	defaultLogger.Log("=== E2E Test Suite Starting ===")

	// Check if we should skip e2e tests
	if os.Getenv("SKIP_E2E") == "true" {
		defaultLogger.Log("SKIP_E2E environment variable is set, skipping e2e tests")
		Skip("Skipping e2e tests")
	}

	// Check if kind is available
	if !isKindAvailable() {
		defaultLogger.Log("ERROR: kind is not available in PATH, skipping e2e tests")
		Skip("kind is not available, skipping e2e tests")
	}
	defaultLogger.Log("✓ kind is available")

	ctx, cancel = context.WithTimeout(context.Background(), TestTimeout)

	// Create and setup kind cluster
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

	// Expose kubeconfig for troubleshooting
	kubeconfigPath := cluster.GetKubeconfigPath()
	defaultLogger.Log("\n=== KUBECONFIG LOCATION ===")
	defaultLogger.Logf("Kubeconfig path: %s", kubeconfigPath)
	defaultLogger.Log("To use this kubeconfig for troubleshooting, run:")
	defaultLogger.Logf("  export KUBECONFIG=%s", kubeconfigPath)
	defaultLogger.Logf("  kubectl --kubeconfig %s get nodes", kubeconfigPath)
	defaultLogger.Log("===========================\n")

	// Deploy test HTTP service
	defaultLogger.Logf("Deploying test HTTP service %s in namespace %s...", TestServiceName, TestNamespace)
	testService, err = NewTestServiceDeployer(cluster.GetKubeconfigPath())
	Expect(err).NotTo(HaveOccurred())
	Expect(testService.Deploy(ctx, TestNamespace, TestServiceName, TestServicePort)).To(Succeed())
	defaultLogger.Log("Test service deployed, waiting for ready...")
	Expect(testService.WaitForReady(ctx, TestNamespace, TestServiceName)).To(Succeed())
	defaultLogger.Log("✓ Test service is ready")

	// Deploy extension server FIRST (before Envoy Gateway)
	By("Deploying extension server")
	defaultLogger.Logf("Deploying extension server in namespace: %s", ExtensionServerNamespace)
	extensionDeployer, err = NewExtensionServerDeployer(cluster.GetKubeconfigPath())
	Expect(err).NotTo(HaveOccurred())
	defaultLogger.Log("✓ Extension server deployer created")

	extensionDeployer.SetCluster(cluster)
	Expect(extensionDeployer.Deploy(ctx, ExtensionServerNamespace)).To(Succeed())
	defaultLogger.Log("✓ Extension server deployed")

	defaultLogger.Log("Waiting for extension server pods to be ready...")
	Expect(extensionDeployer.WaitForReady(ctx, ExtensionServerNamespace)).To(Succeed())
	defaultLogger.Log("✓ Extension server is ready")

	// Install Envoy Gateway with extension server configuration
	By("Installing Envoy Gateway")
	defaultLogger.Logf("Installing Envoy Gateway in namespace: %s", EnvoyGatewayNamespace)
	egInstaller, err = NewEnvoyGatewayInstaller(cluster.GetKubeconfigPath())
	Expect(err).NotTo(HaveOccurred())
	defaultLogger.Log("✓ Envoy Gateway installer created")

	// Configure extension server FQDN for Envoy Gateway
	extensionServerFQDN := fmt.Sprintf("%s.%s.svc.cluster.local", ExtensionServerReleaseName, ExtensionServerNamespace)
	defaultLogger.Logf("Configuring Envoy Gateway with extension server at %s:%d", extensionServerFQDN, ExtensionServerPort)

	Expect(egInstaller.Install(ctx, extensionServerFQDN, ExtensionServerPort)).To(Succeed())
	defaultLogger.Log("✓ Envoy Gateway Helm chart installed")

	defaultLogger.Log("Waiting for Envoy Gateway pods to be ready...")
	Expect(egInstaller.WaitForReady(ctx, EnvoyGatewayNamespace)).To(Succeed())
	defaultLogger.Log("✓ Envoy Gateway is ready")

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
