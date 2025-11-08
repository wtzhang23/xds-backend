package e2e

import (
	"context"
	"os"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var (
	ctx         context.Context
	cancel      context.CancelFunc
	cluster     *KindCluster
	egInstaller *EnvoyGatewayInstaller
)

func TestE2E(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "E2E Suite")
}

var _ = BeforeSuite(func() {
	// Check if we should skip e2e tests
	if os.Getenv("SKIP_E2E") == "true" {
		Skip("Skipping e2e tests")
	}

	// Check if kind is available
	if !isKindAvailable() {
		Skip("kind is not available, skipping e2e tests")
	}

	ctx, cancel = context.WithTimeout(context.Background(), TestTimeout)

	// Create and setup kind cluster
	By("Setting up kind cluster")
	cluster = NewKindCluster(ClusterName)
	Expect(cluster.Create(ctx)).To(Succeed())
	Expect(cluster.WaitForReady(ctx)).To(Succeed())

	// Install Envoy Gateway
	By("Installing Envoy Gateway")
	var err error
	egInstaller, err = NewEnvoyGatewayInstaller(cluster.GetKubeconfigPath())
	Expect(err).NotTo(HaveOccurred())
	Expect(egInstaller.Install(ctx)).To(Succeed())
	Expect(egInstaller.WaitForReady(ctx, EnvoyGatewayNamespace)).To(Succeed())

	// Deploy extension server
	By("Deploying extension server")
	extensionDeployer, err := NewExtensionServerDeployer(cluster.GetKubeconfigPath())
	Expect(err).NotTo(HaveOccurred())
	extensionDeployer.SetCluster(cluster)
	Expect(extensionDeployer.Deploy(ctx, ExtensionServerNamespace)).To(Succeed())
	Expect(extensionDeployer.WaitForReady(ctx, ExtensionServerNamespace)).To(Succeed())
})

var _ = AfterSuite(func() {
	if cancel != nil {
		cancel()
	}

	if cluster != nil {
		By("Cleaning up kind cluster")
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		_ = cluster.Delete(ctx)
	}
})
