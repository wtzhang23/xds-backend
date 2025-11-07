package types

import (
	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
)

type XdsBackendHandler interface {
	Kind() string
	ApiVersion() string
	ParseBackendFromBytes(data []byte) (XdsBackendConfig, error)
}

type XdsBackendConfig interface {
	EdsServiceName() string
	XdsServerName() string
}

type XdsConfigSource interface {
	GetConfigSource() *corev3.ConfigSource
	GetCluster() *clusterv3.Cluster
}

var _ XdsConfigSource = (*XdsConfigSourceImpl)(nil)

type XdsConfigSourceImpl struct {
	ExplicitConfigSource *corev3.ConfigSource
	ExplicitCluster      *clusterv3.Cluster
}

func (s *XdsConfigSourceImpl) GetConfigSource() *corev3.ConfigSource {
	return s.ExplicitConfigSource
}

func (s *XdsConfigSourceImpl) GetCluster() *clusterv3.Cluster {
	return s.ExplicitCluster
}
