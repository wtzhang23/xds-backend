package v1alpha1

import (
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	"github.com/wtzhang23/xds-backend/pkg/types"
	"google.golang.org/protobuf/types/known/durationpb"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	gwapiv1 "sigs.k8s.io/gateway-api/apis/v1"
)

var _ types.XdsBackendConfig = (*XdsBackend)(nil)

// XdsBackend is a custom [BackendRef](https://gateway.envoyproxy.io/docs/api/extension_types/#backendref)
// for Envoy Gateway that allows configuring backends to use an external EDS server for endpoint discovery.
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced
// +kubebuilder:subresource:status
type XdsBackend struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// XdsBackendSpec defines the xDS server and resource to use to fetch endpoints for the backend.
	Spec XdsBackendSpec `json:"spec,omitempty"`
}

// XdsBackendSpec defines the xDS server and resource to use to fetch endpoints for the backend.
// +kubebuilder:object:generate=true
type XdsBackendSpec struct {
	// Endpoints configures how to fetch endpoints for the backend.
	// +kubebuilder:validation:Required
	Endpoints *XdsConfigSource `json:"endpoints,omitempty"`

	// Tls configures how to configure TLS for the backend.
	// +optional
	Tls *TlsSettings `json:"tls,omitempty"`
}

// TlsSettings configures the TLS settings for the backend.
// +kubebuilder:object:generate=true
type TlsSettings struct {
	// CaCertificates configures the source of truth for CA certificates.
	// +optional
	CaCertificates []*XdsConfigSource `json:"caCertificates,omitempty"`
	// Hostname configures the hostname to use for TLS.
	// +optional
	Hostname *gwapiv1.Hostname `json:"hostname,omitempty"`
}

// XdsConfigSource configures where to fetch xDS resources from.
// +kubebuilder:object:generate=true
type XdsConfigSource struct {
	// Server is the name of the xDS server to use to fetch xDS resources from. This name
	// must match the name configured in the xDS backend extension server.
	// +optional
	Server *ServerConfigSource `json:"server,omitempty"`

	// Path is the path to the file to fetch xDS resources from. This should be used
	// only to fetch xDS resources from a local file.
	// +optional
	Path *PathConfigSource `json:"path,omitempty"`

	// ApiType is the protocol to use to fetch endpoints for the backend.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Enum=GRPC;DELTA_GRPC;REST
	// +kubebuilder:default=GRPC
	ApiType ApiType `json:"apiType"`

	// Name is the xDS resourse name.
	// +kubebuilder:validation:Required
	Name string `json:"name"`
}

// ApiType is the protocol to use to fetch endpoints for the backend.
// +kubebuilder:validation:Required
// +kubebuilder:validation:Enum=GRPC;DELTA_GRPC;REST
// +kubebuilder:default=GRPC
type ApiType string

const (
	ApiTypeGrpc      ApiType = "GRPC"
	ApiTypeDeltaGrpc ApiType = "DELTA_GRPC"
	ApiTypeRest      ApiType = "REST"
)

// PathConfigSource is a config source that fetches endpoints for the backend from a local file.
// +kubebuilder:object:generate=true
type PathConfigSource struct {
	// Path is a path to the file to fetch endpoints for the backend. This should be used
	// only to fetch endpoints for the backend from a local file.
	// +kubebuilder:validation:Required
	Path string `json:"path,omitempty"`

	// WatchedDir is a directory to watch for changes. If not specified, the directory will be
	// watched for changes.
	// +optional
	WatchedDir string `json:"watchedDir,omitempty"`
}

// ServerConfigSource is a config source that fetches endpoints for the backend from a remote xDS server.
// +kubebuilder:object:generate=true
type ServerConfigSource struct {
	// Server is the name of the xDS server to use to fetch endpoints for the backend. This name
	// must match the name configured in the xDS backend extension server.
	// +kubebuilder:validation:Required
	Server string `json:"server"`
}

func (s *XdsConfigSource) GetConfigSource() types.XdsConfigSource {
	version := corev3.ApiVersion_V3 // Do not support other API versions for now
	cfgSource := &corev3.ConfigSource{
		ResourceApiVersion: version,
	}
	switch {
	case s.Server != nil:
		apiType := corev3.ApiConfigSource_GRPC
		switch s.ApiType {
		case ApiTypeGrpc:
			apiType = corev3.ApiConfigSource_GRPC
		case ApiTypeDeltaGrpc:
			apiType = corev3.ApiConfigSource_DELTA_GRPC
		case ApiTypeRest:
			apiType = corev3.ApiConfigSource_REST
		}
		cfgSource.ConfigSourceSpecifier = &corev3.ConfigSource_ApiConfigSource{
			ApiConfigSource: &corev3.ApiConfigSource{
				ApiType:             apiType,
				TransportApiVersion: version,
				GrpcServices: []*corev3.GrpcService{
					{
						TargetSpecifier: &corev3.GrpcService_EnvoyGrpc_{
							EnvoyGrpc: &corev3.GrpcService_EnvoyGrpc{
								ClusterName: s.Server.Server,
							},
						},
					},
				},
				RefreshDelay: &durationpb.Duration{
					Seconds: 10,
				},
			},
		}
	case s.Path != nil:
		var watchedDir *corev3.WatchedDirectory
		if s.Path.WatchedDir != "" {
			watchedDir = &corev3.WatchedDirectory{
				Path: s.Path.WatchedDir,
			}
		}
		cfgSource.ConfigSourceSpecifier = &corev3.ConfigSource_PathConfigSource{
			PathConfigSource: &corev3.PathConfigSource{
				Path:             s.Path.Path,
				WatchedDirectory: watchedDir,
			},
		}
	default:
		// Use ADS of envoy gateway itself to fetch endpoints for the backend
		cfgSource.ConfigSourceSpecifier = &corev3.ConfigSource_Ads{
			Ads: &corev3.AggregatedConfigSource{},
		}
	}
	return types.XdsConfigSource{
		Name:         s.Name,
		ConfigSource: cfgSource,
	}
}

func (b *XdsBackend) GetEndpointsSource() types.XdsConfigSource {
	return b.Spec.Endpoints.GetConfigSource()
}

func (b *XdsBackend) GetTlsSettings() *types.XdsTlsSettings {
	if b.Spec.Tls == nil {
		return nil
	}
	caCertificates := make([]types.XdsConfigSource, len(b.Spec.Tls.CaCertificates))
	for i, caCertificate := range b.Spec.Tls.CaCertificates {
		caCertificates[i] = caCertificate.GetConfigSource()
	}
	return &types.XdsTlsSettings{
		CaCertificates: caCertificates,
		Hostname:       b.Spec.Tls.Hostname,
	}
}
