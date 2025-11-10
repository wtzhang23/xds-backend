package e2e

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/api/errors"
)

// TestServiceDeployer handles deployment of a test HTTP service
type TestServiceDeployer struct {
	k8sClient *K8sClient
}

// NewTestServiceDeployer creates a new test service deployer
func NewTestServiceDeployer(kubeconfig string) (*TestServiceDeployer, error) {
	k8sClient, err := NewK8sClient(kubeconfig)
	if err != nil {
		return nil, err
	}
	return &TestServiceDeployer{
		k8sClient: k8sClient,
	}, nil
}

// Deploy deploys a test HTTP service
func (d *TestServiceDeployer) Deploy(ctx context.Context, namespace, name string, port int) error {
	// Create namespace if needed
	if err := d.k8sClient.CreateNamespace(ctx, namespace); err != nil {
		return fmt.Errorf("failed to create namespace: %w", err)
	}

	// Load deployment template
	templateData := TemplateData{
		TestServiceName: name,
		TestNamespace:   namespace,
		TestServicePort: port,
	}
	deploymentTemplatePath := GetTemplatePath("test-service-deployment.yaml")
	deploymentYaml, err := LoadTemplate(deploymentTemplatePath, templateData)
	if err != nil {
		return fmt.Errorf("failed to load deployment template: %w", err)
	}
	if err := d.k8sClient.ApplyYAML(ctx, deploymentYaml); err != nil {
		if !errors.IsAlreadyExists(err) {
			return fmt.Errorf("failed to apply deployment: %w", err)
		}
	}

	// Load service template
	serviceTemplatePath := GetTemplatePath("test-service-service.yaml")
	serviceYaml, err := LoadTemplate(serviceTemplatePath, templateData)
	if err != nil {
		return fmt.Errorf("failed to load service template: %w", err)
	}
	if err := d.k8sClient.ApplyYAML(ctx, serviceYaml); err != nil {
		if !errors.IsAlreadyExists(err) {
			return fmt.Errorf("failed to apply service: %w", err)
		}
	}

	return nil
}

// WaitForReady waits for the test service to be ready
func (d *TestServiceDeployer) WaitForReady(ctx context.Context, namespace, name string) error {
	labelSelector := fmt.Sprintf("app=%s", name)
	return d.k8sClient.WaitForPodsReady(ctx, namespace, labelSelector, TestTimeout)
}

// GetServiceAddress returns the service address
func (d *TestServiceDeployer) GetServiceAddress(namespace, name string) string {
	return fmt.Sprintf("%s.%s.svc.cluster.local", name, namespace)
}

// GetServiceClusterIP gets the cluster IP of the service
func (d *TestServiceDeployer) GetServiceClusterIP(ctx context.Context, namespace, name string) (string, error) {
	return d.k8sClient.GetServiceClusterIP(ctx, namespace, name)
}
