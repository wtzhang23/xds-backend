package v1alpha1

import (
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	"github.com/wtzhang23/xds-backend/pkg/types"
	"google.golang.org/protobuf/types/known/durationpb"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
	// Service is the name of the service to use to fetch endpoints for the backend.
	// If not specified, the default name used by Envoy Gateway for the backend will be used
	// when requesting the endpoints from the xDS server.
	//
	// +optional
	Service string `json:"service,omitempty"`

	// ApiType is the protocol to use to fetch endpoints for the backend.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Enum=GRPC;DELTA_GRPC;REST
	// +kubebuilder:default=GRPC
	ApiType ApiType `json:"apiType"`

	// Server is the name of the xDS server to use to fetch endpoints for the backend. This name
	// must match the name configured in the xDS backend extension server.
	// +optional
	Server *ServerConfigSource `json:"server,omitempty"`

	// Path is the path to the file to fetch endpoints for the backend. This should be used
	// only to fetch endpoints for the backend from a local file.
	// +optional
	Path *PathConfigSource `json:"path,omitempty"`
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

func (b *XdsBackend) EdsServiceName() string {
	return b.Spec.Service
}

func (b *XdsBackend) GetConfigSource() *corev3.ConfigSource {
	version := corev3.ApiVersion_V3 // Do not support other API versions for now
	rv := &corev3.ConfigSource{
		ResourceApiVersion: version,
	}
	switch {
	case b.Spec.Server != nil:
		apiType := corev3.ApiConfigSource_GRPC
		switch b.Spec.ApiType {
		case ApiTypeGrpc:
			apiType = corev3.ApiConfigSource_GRPC
		case ApiTypeDeltaGrpc:
			apiType = corev3.ApiConfigSource_DELTA_GRPC
		case ApiTypeRest:
			apiType = corev3.ApiConfigSource_REST
		}
		rv.ConfigSourceSpecifier = &corev3.ConfigSource_ApiConfigSource{
			ApiConfigSource: &corev3.ApiConfigSource{
				ApiType:             apiType,
				TransportApiVersion: version,
				GrpcServices: []*corev3.GrpcService{
					{
						TargetSpecifier: &corev3.GrpcService_EnvoyGrpc_{
							EnvoyGrpc: &corev3.GrpcService_EnvoyGrpc{
								ClusterName: b.Spec.Server.Server,
							},
						},
					},
				},
				RefreshDelay: &durationpb.Duration{
					Seconds: 10,
				},
			},
		}
	case b.Spec.Path != nil:
		var watchedDir *corev3.WatchedDirectory
		if b.Spec.Path.WatchedDir != "" {
			watchedDir = &corev3.WatchedDirectory{
				Path: b.Spec.Path.WatchedDir,
			}
		}
		rv.ConfigSourceSpecifier = &corev3.ConfigSource_PathConfigSource{
			PathConfigSource: &corev3.PathConfigSource{
				Path:             b.Spec.Path.Path,
				WatchedDirectory: watchedDir,
			},
		}
	default:
		// Use ADS of envoy gateway itself to fetch endpoints for the backend
		rv.ConfigSourceSpecifier = &corev3.ConfigSource_Ads{
			Ads: &corev3.AggregatedConfigSource{},
		}
	}
	return rv
}
