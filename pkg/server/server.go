package server

import (
	"context"
	"encoding/json"
	"log/slog"

	pb "github.com/envoyproxy/gateway/proto/extension"
	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/wtzhang23/xds-backend/pkg/types"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
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
	s.log.DebugContext(ctx, "Configuring cluster to use EDS", slog.String("cluster", cluster.Name), slog.String("eds_service_name", backendConfig.EdsServiceName()))
	cluster.ClusterDiscoveryType = &clusterv3.Cluster_Type{
		Type: clusterv3.Cluster_EDS,
	}
	cluster.EdsClusterConfig = &clusterv3.Cluster_EdsClusterConfig{
		ServiceName: backendConfig.EdsServiceName(),
		EdsConfig:   backendConfig.GetConfigSource(),
	}
	s.log.DebugContext(ctx, "Cluster configured for EDS", slog.String("cluster", cluster.Name), slog.String("eds_service_name", backendConfig.EdsServiceName()))
}
