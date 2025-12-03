package e2e

import "time"

const (
	// ClusterName is the name of the kind cluster used for testing
	ClusterName = "xds-backend-e2e"
	// EnvoyGatewayNamespace is the namespace where Envoy Gateway is installed
	EnvoyGatewayNamespace = "envoy-gateway-system"
	// ExtensionServerNamespace is the namespace where the extension server is deployed
	ExtensionServerNamespace = "xds-backend-system"
	// TestNamespace is the namespace for test resources
	TestNamespace = "test-namespace"

	// TestTimeout is the default timeout for test operations
	TestTimeout = 5 * time.Minute
	// TestPollInterval is the interval for polling operations
	TestPollInterval = 2 * time.Second
	// HelmTimeout is the timeout for Helm operations
	HelmTimeout = 5 * time.Minute
	// CleanupTimeout is the timeout for cleanup operations
	CleanupTimeout = 2 * time.Minute
	// DeploymentTimeout is the timeout for deployment operations
	DeploymentTimeout = 2 * time.Minute
	// StabilizeTimeout is the timeout for waiting for system pods to stabilize
	StabilizeTimeout = 10 * time.Second
	// LogExtractionTimeout is the timeout for extracting logs
	LogExtractionTimeout = 30 * time.Second
	// ClusterReadyTimeout is the timeout for waiting for cluster to be ready
	ClusterReadyTimeout = 5 * time.Minute
	// HTTPClientTimeout is the timeout for HTTP client requests
	HTTPClientTimeout = 10 * time.Second
	// EnvoyProxyProcessingDelay is the delay to allow EnvoyProxy controller to process changes
	EnvoyProxyProcessingDelay = 3 * time.Second
	// EnvoyEndpointProcessingDelay is the delay to allow Envoy to process endpoints after cluster configuration
	EnvoyEndpointProcessingDelay = 2 * time.Second
	// EnvoyAccessLogFlushDelay is the delay to allow Envoy to flush access logs before collection
	EnvoyAccessLogFlushDelay = 3 * time.Second
	// MetricsCollectionDelay is the delay to allow metrics to be collected
	MetricsCollectionDelay = 2 * time.Second
	// EnvoyAdminAPIReadyDelay is the delay to allow Envoy admin API to be ready after port forwarding
	EnvoyAdminAPIReadyDelay = 2 * time.Second
	// HTTPRequestTimeout is the timeout for retrying HTTP requests until success
	HTTPRequestTimeout = 30 * time.Second

	// ExtensionServerImageRepo is the Docker image repository for the extension server
	ExtensionServerImageRepo = "wtzhang23/xds-backend-extension-server"
	// ExtensionServerImageTag is the Docker image tag for the extension server
	ExtensionServerImageTag = "latest"
	// ImagePullPolicy is the image pull policy for test deployments
	ImagePullPolicy = "Never"

	// EnvoyGatewayReleaseName is the Helm release name for Envoy Gateway
	EnvoyGatewayReleaseName = "eg"
	// ExtensionServerReleaseName is the Helm release name for the extension server
	ExtensionServerReleaseName = "xds-backend"

	// EnvoyGatewayLabelSelector is the label selector for Envoy Gateway pods
	EnvoyGatewayLabelSelector = "app.kubernetes.io/name=gateway-helm"
	// ExtensionServerLabelSelector is the label selector for extension server pods
	ExtensionServerLabelSelector = "app.kubernetes.io/name=xds-backend"

	// TestServiceName is the name of the test HTTP service
	TestServiceName = "test-http-service"
	// TestServicePort is the port for the test HTTP service (plaintext)
	TestServicePort = 8080
	// TestServiceTLSPort is the port for the test HTTP service (TLS)
	TestServiceTLSPort = 8443
	// EnvoyGatewayHostPort is the host port for Envoy Gateway HTTP
	EnvoyGatewayHostPort = 8080
	// EnvoyGatewayContainerPort is the container port for Envoy Gateway HTTP
	EnvoyGatewayContainerPort = 80
	// EnvoyGatewayHTTPSHostPort is the host port for Envoy Gateway HTTPS
	EnvoyGatewayHTTPSHostPort = 8443
	// EnvoyGatewayHTTPSContainerPort is the container port for Envoy Gateway HTTPS
	EnvoyGatewayHTTPSContainerPort = 443

	// EnvoyGatewayChartPath is the OCI path for the Envoy Gateway Helm chart
	EnvoyGatewayChartPath = "oci://docker.io/envoyproxy/gateway-helm"
	// EnvoyGatewayChartVersion is the version of the Envoy Gateway chart
	EnvoyGatewayChartVersion = "1.6.0"

	// HelmChartDir is the directory for Helm charts
	HelmChartDir = ".helm/charts"
	// HelmRepositoryDir is the directory for Helm repository cache
	HelmRepositoryDir = ".helm/repository"
	// HelmRepositoryConfig is the path to Helm repository config
	HelmRepositoryConfig = ".helm/repositories.yaml"
	// KubeconfigDir is the directory for kubeconfig files
	KubeconfigDir = ".kubeconfig"

	// XdsBackendGroup is the API group for XdsBackend CRD
	XdsBackendGroup = "xdsbackend.wtzhang23.github.io"
	// XdsBackendAPIVersion is the API version for XdsBackend CRD
	XdsBackendAPIVersion = "v1alpha1"
	// XdsBackendKind is the kind name for XdsBackend CRD
	XdsBackendKind = "XdsBackend"

	// GatewayName is the name of the Gateway resource
	GatewayName = "eg"
	// GatewayClassName is the name of the GatewayClass resource
	GatewayClassName = "eg"
	// HTTPRouteName is the name of the HTTPRoute resource for file-based EDS test
	HTTPRouteName = "test-route-file"
	// FileEdsHTTPRouteName is the name of the HTTPRoute resource for fileeds test
	FileEdsHTTPRouteName = "test-route-eds"
	// XdsBackendResourceName is the name of the XdsBackend resource for file-based EDS test
	XdsBackendResourceName = "test-backend-file"
	// FileEdsXdsBackendResourceName is the name of the XdsBackend resource for fileeds test
	FileEdsXdsBackendResourceName = "test-backend-eds"
	// ExpectedClusterName is the expected Envoy cluster name for the HTTPRoute
	ExpectedClusterName = "httproute/envoy-gateway-system/test-route-file/rule/0"
	// FileEdsExpectedClusterName is the expected Envoy cluster name for the fileeds HTTPRoute
	FileEdsExpectedClusterName = "httproute/envoy-gateway-system/test-route-eds/rule/0"
	// HTTPRoutePathPrefixFile is the path prefix for the file-based EDS HTTPRoute
	HTTPRoutePathPrefixFile = "/file"
	// HTTPRoutePathPrefixEds is the path prefix for the fileeds HTTPRoute
	HTTPRoutePathPrefixEds = "/eds"

	// GatewayListenerName is the name of the Gateway listener
	GatewayListenerName = "http"
	// GatewayListenerPort is the port number for the Gateway listener
	GatewayListenerPort = 80

	// EnvoyAdminPort is the Envoy admin API port
	EnvoyAdminPort = 19000
	// EnvoyAdminPortForwardPort is the local port for forwarding to Envoy admin API
	EnvoyAdminPortForwardPort = 19001
	// EnvoyAdminConfigDumpPath is the path for Envoy admin config dump endpoint
	EnvoyAdminConfigDumpPath = "/config_dump?include_eds"

	// ExtensionServerPort is the gRPC port for the extension server
	ExtensionServerPort = 5005
	// ExtensionServerTLSPort is the TLS gRPC port for the extension server
	ExtensionServerTLSPort = 5006
	// ExtensionServerMetricsPort is the metrics port for the extension server
	ExtensionServerMetricsPort = 8081

	// EnvoyProxyOwningGatewayLabelKey is the label key for the owning gateway name
	EnvoyProxyOwningGatewayLabelKey = "gateway.envoyproxy.io/owning-gateway-name"
	// EnvoyProxyComponentLabelKey is the label key for the component
	EnvoyProxyComponentLabelKey = "app.kubernetes.io/component"
	// EnvoyProxyComponentLabelValue is the label value for the proxy component
	EnvoyProxyComponentLabelValue = "proxy"

	// LogDir is the directory for test logs
	LogDir = ".logs"
	// LogTimestampFormat is the timestamp format for log filenames
	LogTimestampFormat = "20060102-150405"

	// EdsConfigMapName is the name of the ConfigMap containing EDS configuration
	EdsConfigMapName = "eg-eds-config"
	// EdsConfigPath is the path to the EDS config file in the Envoy pod
	EdsConfigPath = "/etc/envoy/eds/eds-config.yaml"
	// EnvoyProxyPodPort is the port that Envoy proxy listens on in the pod
	EnvoyProxyPodPort = 10080

	// FileEdsDeploymentName is the name of the fileeds deployment
	FileEdsDeploymentName = "fileeds"
	// FileEdsServiceName is the name of the fileeds service
	FileEdsServiceName = "fileeds"
	// FileEdsConfigMapName is the name of the ConfigMap containing EDS configuration for fileeds
	FileEdsConfigMapName = "fileeds-eds-config"
	// FileEdsPort is the gRPC port for the fileeds server
	FileEdsPort = 5006
	// FileEdsConfigPath is the path to the EDS config file in the fileeds pod
	FileEdsConfigPath = "/etc/envoy/eds/eds-config.yaml"
	// FileEdsConfigDir is the directory where the EDS config file is mounted
	FileEdsConfigDir = "/etc/envoy/eds"
	// FileEdsClusterName is the name of the static cluster for fileeds in Envoy bootstrap
	FileEdsClusterName = "fileeds-server"

	// BackendTLSPolicyTest constants
	BackendTLSPolicyHTTPRouteName  = "test-route-backend-tls"
	BackendTLSPolicyXdsBackendName = "test-backend-tls"
	BackendTLSPolicyName           = "test-backend-tls-policy"
	BackendTLSPolicyPathPrefix     = "/backend-tls"
	GatewayTLSListenerName         = "https"
	GatewayTLSListenerPort         = 443
	GatewayTLSCertificateName      = "gateway-tls-cert"
	BackendTLSCACertName           = "backend-tls-ca-cert"
	BackendTLSHostname             = "test-http-service.test-namespace.svc.cluster.local"
	TestServiceTLSSecretName       = "test-service-tls"
	BackendTLSEdsConfigMapName     = "backend-tls-eds-config"
	BackendTLSEdsConfigPath        = "/etc/envoy/eds/backend-tls-eds-config.yaml"
)
