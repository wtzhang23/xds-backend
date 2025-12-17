package server

import (
	"context"
	"encoding/json"
	"log/slog"

	pb "github.com/envoyproxy/gateway/proto/extension"
	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	tlsv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/transport_sockets/tls/v3"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/wtzhang23/xds-backend/pkg/types"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

type handlerKey struct {
	kind       string
	apiVersion string
}

var (
	postClusterModifyTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "xds_backend_post_cluster_modify_total",
			Help: "Total number of PostClusterModify calls",
		},
		[]string{"cluster_name", "status"},
	)
	postClusterModifyErrors = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "xds_backend_post_cluster_modify_errors_total",
			Help: "Total number of PostClusterModify errors",
		},
		[]string{"cluster_name", "error_type"},
	)
)

func init() {
	// Register metrics with controller-runtime metrics registry
	metrics.Registry.MustRegister(postClusterModifyTotal)
	metrics.Registry.MustRegister(postClusterModifyErrors)
}

type Server struct {
	pb.UnimplementedEnvoyGatewayExtensionServer
	handlers map[handlerKey]types.XdsBackendHandler
	log      *slog.Logger
}

func NewServer(log *slog.Logger, handlers ...types.XdsBackendHandler) *Server {
	handlersMap := make(map[handlerKey]types.XdsBackendHandler)
	for _, handler := range handlers {
		key := handlerKey{kind: handler.Kind(), apiVersion: handler.ApiVersion()}
		handlersMap[key] = handler
	}
	return &Server{
		handlers: handlersMap,
		log:      log,
	}
}

// processBackendExtensionResource processes a single backend extension resource
func (s *Server) processBackendExtensionResource(ctx context.Context, cluster *clusterv3.Cluster, ext *pb.ExtensionResource) error {
	var resourceInfo map[string]any
	if err := json.Unmarshal(ext.GetUnstructuredBytes(), &resourceInfo); err != nil {
		return status.Errorf(codes.InvalidArgument, "failed to unmarshal backend extension resource: %v", err)
	}

	kind, ok := resourceInfo["kind"].(string)
	if !ok {
		return status.Error(codes.InvalidArgument, "kind is required")
	}

	apiVersion, ok := resourceInfo["apiVersion"].(string)
	if !ok {
		return status.Error(codes.InvalidArgument, "apiVersion is required")
	}

	key := handlerKey{kind: kind, apiVersion: apiVersion}
	handler, ok := s.handlers[key]
	if !ok {
		s.log.DebugContext(ctx, "Handler not found", slog.String("kind", kind), slog.String("apiVersion", apiVersion))
		return nil // Not an error, just skip this resource
	}

	s.log.DebugContext(ctx, "Handler found", slog.String("kind", kind), slog.String("apiVersion", apiVersion))
	backendConfig, err := handler.ParseBackendFromBytes(ext.GetUnstructuredBytes())
	if err != nil {
		return status.Errorf(codes.Internal, "failed to parse backend config from bytes: %v", err)
	}

	s.changeXdsOfCluster(ctx, cluster, backendConfig)
	s.configureTLSForCluster(ctx, cluster, backendConfig)
	return nil
}

func (s *Server) PostClusterModify(ctx context.Context, req *pb.PostClusterModifyRequest) (*pb.PostClusterModifyResponse, error) {
	cluster := req.Cluster
	clusterName := cluster.Name
	s.log.DebugContext(ctx, "PostClusterModify callback invoked", slog.String("cluster", clusterName))

	// handlePostClusterModifyError increments error metrics and returns the error
	// It determines the error type from the status code
	handlePostClusterModifyError := func(err error) error {
		var errorType string
		if st, ok := status.FromError(err); ok {
			switch st.Code() {
			case codes.InvalidArgument:
				errorType = "invalid_argument"
			case codes.Internal:
				errorType = "parse_error"
			default:
				errorType = "unknown_error"
			}
		} else {
			err = status.Errorf(codes.Internal, "unknown error: %v", err)
		}

		postClusterModifyErrors.WithLabelValues(clusterName, errorType).Inc()
		postClusterModifyTotal.WithLabelValues(clusterName, "error").Inc()
		return err
	}

	for _, ext := range req.PostClusterContext.BackendExtensionResources {
		if err := s.processBackendExtensionResource(ctx, cluster, ext); err != nil {
			return nil, handlePostClusterModifyError(err)
		}
	}

	postClusterModifyTotal.WithLabelValues(clusterName, "success").Inc()
	return &pb.PostClusterModifyResponse{
		Cluster: cluster,
	}, nil
}

func (s *Server) changeXdsOfCluster(ctx context.Context, cluster *clusterv3.Cluster, backendConfig types.XdsBackendConfig) {
	edsSource := backendConfig.GetEndpointsSource()
	cluster.ClusterDiscoveryType = &clusterv3.Cluster_Type{
		Type: clusterv3.Cluster_EDS,
	}
	cluster.EdsClusterConfig = &clusterv3.Cluster_EdsClusterConfig{
		ServiceName: edsSource.Name,
		EdsConfig:   edsSource.ConfigSource,
	}
	s.log.DebugContext(ctx, "Cluster configured for EDS", slog.String("cluster", cluster.Name), slog.String("eds_service_name", edsSource.Name))
}

func (s *Server) configureTLSForCluster(ctx context.Context, cluster *clusterv3.Cluster, backendConfig types.XdsBackendConfig) {
	tlsSettings := backendConfig.GetTlsSettings()
	if tlsSettings == nil {
		return
	}

	newUpstreamTlsContext := func() *tlsv3.UpstreamTlsContext {
		if cluster.GetTransportSocket().GetName() != "envoy.transport_sockets.tls" {
			s.log.DebugContext(ctx, "Replacing existing TransportSocket with TLS", slog.String("cluster", cluster.Name))
			return nil
		}
		if cluster.TransportSocket.GetConfigType() == nil {
			s.log.ErrorContext(ctx, "Missing TransportSocket config", slog.String("cluster", cluster.Name))
			return nil
		}
		configType, ok := cluster.TransportSocket.GetConfigType().(*corev3.TransportSocket_TypedConfig)
		if !ok {
			s.log.ErrorContext(ctx, "Missing TypedConfig", slog.String("cluster", cluster.Name))
			return nil
		}
		newTlsSettings := &tlsv3.UpstreamTlsContext{}
		if err := anypb.UnmarshalTo(configType.TypedConfig, newTlsSettings, proto.UnmarshalOptions{}); err != nil {
			s.log.ErrorContext(ctx, "Failed to unmarshal TransportSocket", slog.String("error", err.Error()), slog.String("cluster", cluster.Name))
			return nil
		}
		return newTlsSettings
	}()
	if newUpstreamTlsContext == nil {
		newUpstreamTlsContext = &tlsv3.UpstreamTlsContext{}
	}

	// Merge TLS settings
	if tlsSettings.Hostname != nil {
		newUpstreamTlsContext.Sni = string(*tlsSettings.Hostname)
	}

	addedCertName := "<none>"
	if tlsSettings.CaCertificates != nil {
		if newUpstreamTlsContext.CommonTlsContext == nil {
			newUpstreamTlsContext.CommonTlsContext = &tlsv3.CommonTlsContext{}
		}

		validationContextSdsConfig := &tlsv3.SdsSecretConfig{
			Name:      tlsSettings.CaCertificates.Name,
			SdsConfig: tlsSettings.CaCertificates.ConfigSource,
		}
		newUpstreamTlsContext.CommonTlsContext.ValidationContextType = &tlsv3.CommonTlsContext_ValidationContextSdsSecretConfig{
			ValidationContextSdsSecretConfig: validationContextSdsConfig,
		}
		addedCertName = tlsSettings.CaCertificates.Name
	}

	// Add new TLS settings to cluster
	typedConfig, err := anypb.New(newUpstreamTlsContext)
	if err != nil {
		s.log.ErrorContext(ctx, "Failed to marshal TransportSocket", slog.String("error", err.Error()), slog.String("cluster", cluster.Name))
		return
	}
	cluster.TransportSocket = &corev3.TransportSocket{
		Name: "envoy.transport_sockets.tls",
		ConfigType: &corev3.TransportSocket_TypedConfig{
			TypedConfig: typedConfig,
		},
	}
	s.log.DebugContext(ctx, "Cluster configured for TLS", slog.String("cluster", cluster.Name), slog.String("added_cert", addedCertName))
}
