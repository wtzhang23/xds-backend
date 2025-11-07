package v1alpha1

import (
	"github.com/wtzhang23/xds-backend/pkg/types"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var _ types.XdsBackendConfig = (*XdsBackend)(nil)

// XdsBackend is a custom [BackendRef](https://gateway.envoyproxy.io/docs/api/extension_types/#backendref)
// for Envoy Gateway that allows configuring backends to use an external EDS server for endpoint discovery.
type XdsBackend struct {
	metav1.TypeMeta `json:",inline"`

	// XdsBackendSpec defines the xDS server and resource to use to fetch endpoints for the backend.
	XdsBackendSpec `json:",inline"`
}

// XdsBackendSpec defines the xDS server and resource to use to fetch endpoints for the backend.
type XdsBackendSpec struct {
	// Server is the name of the xDS server to use to fetch endpoints for the backend. This name
	// must match the name configured in the xDS backend extension server.
	Server string `json:"server"`

	// Service is the name of the service to use to fetch endpoints for the backend.
	// If not specified, the default name used by Envoy Gateway for the backend will be used
	// when requesting the endpoints from the xDS server.
	//
	// +optional
	Service string `json:"service"`
}

func (b *XdsBackend) EdsServiceName() string {
	return b.Service
}

func (b *XdsBackend) XdsServerName() string {
	return b.Server
}
