package types

import (
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
)

type XdsBackendHandler interface {
	Kind() string
	ApiVersion() string
	ParseBackendFromBytes(data []byte) (XdsBackendConfig, error)
}

type XdsBackendConfig interface {
	EdsServiceName() string
	GetConfigSource() *corev3.ConfigSource
}
