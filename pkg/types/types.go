package types

import (
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	gwapiv1 "sigs.k8s.io/gateway-api/apis/v1"
)

type XdsBackendHandler interface {
	Kind() string
	ApiVersion() string
	ParseBackendFromBytes(data []byte) (XdsBackendConfig, error)
}

type XdsBackendConfig interface {
	GetEndpointsSource() XdsConfigSource
	GetTlsSettings() *XdsTlsSettings
}

type XdsConfigSource struct {
	Name         string
	ConfigSource *corev3.ConfigSource
}

type XdsTlsSettings struct {
	CaCertificates *XdsConfigSource
	Hostname       *gwapiv1.Hostname
}
