package e2e

import (
	"context"
	"fmt"
	"net/http"
	"net/url"

	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"
)

// PortForwarder handles port forwarding to a Kubernetes pod
type PortForwarder struct {
	stopChan chan struct{}
	cleanup  func()
}

// Stop stops the port forward
func (pf *PortForwarder) Stop() {
	if pf.stopChan != nil {
		close(pf.stopChan)
	}
	if pf.cleanup != nil {
		pf.cleanup()
	}
}

// SetupPortForward sets up port forwarding to a pod and returns a PortForwarder that can be stopped
// portMappings should be in the format "localPort:remotePort" (e.g., "8080:8080")
func SetupPortForward(ctx context.Context, restConfig *rest.Config, namespace, podName string, portMappings []string) (*PortForwarder, error) {
	path := fmt.Sprintf("/api/v1/namespaces/%s/pods/%s/portforward", namespace, podName)
	transport, upgrader, err := spdy.RoundTripperFor(restConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create transport: %w", err)
	}

	baseURL, err := url.Parse(restConfig.Host)
	if err != nil {
		return nil, fmt.Errorf("failed to parse host: %w", err)
	}
	baseURL.Path = path

	dialer := spdy.NewDialer(upgrader, &http.Client{Transport: transport}, "POST", baseURL)

	stopChan := make(chan struct{}, 1)
	readyChan := make(chan struct{}, 1)
	errChan := make(chan error, 1)

	fw, err := portforward.New(dialer, portMappings, stopChan, readyChan, nil, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create port forward: %w", err)
	}

	go func() {
		if err := fw.ForwardPorts(); err != nil {
			select {
			case errChan <- err:
			default:
				// Error channel full, ignore to avoid blocking
			}
		}
	}()

	// Wait for port forward to be ready
	select {
	case <-ctx.Done():
		close(stopChan)
		return nil, fmt.Errorf("context cancelled: %w", ctx.Err())
	case err := <-errChan:
		close(stopChan)
		return nil, fmt.Errorf("port forward error: %w", err)
	case <-readyChan:
		// Port forward is ready
	}

	return &PortForwarder{
		stopChan: stopChan,
		cleanup: func() {
			// Additional cleanup if needed
		},
	}, nil
}

// SetupPortForwardWithGomega sets up port forwarding using Gomega expectations (for use in tests)
// This function should be called from test files that have Gomega imported
func SetupPortForwardWithGomega(ctx context.Context, restConfig *rest.Config, namespace, podName string, portMappings []string) (*PortForwarder, error) {
	return SetupPortForward(ctx, restConfig, namespace, podName, portMappings)
}
