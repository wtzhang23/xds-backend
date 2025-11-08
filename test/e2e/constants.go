package e2e

import "time"

const (
	// ClusterName is the name of the kind cluster used for testing
	ClusterName = "xds-backend-e2e"
	// EnvoyGatewayNamespace is the namespace where Envoy Gateway is installed
	EnvoyGatewayNamespace = "envoy-gateway-system"
	// ExtensionServerNamespace is the namespace where the extension server is deployed
	ExtensionServerNamespace = "xds-backend-system"
	// TestTimeout is the default timeout for test operations
	TestTimeout = 5 * time.Minute
	// TestPollInterval is the interval for polling operations
	TestPollInterval = 2 * time.Second
)
