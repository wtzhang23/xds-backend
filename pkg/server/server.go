package server

import (
	"context"
	"encoding/json"
	"log/slog"

	pb "github.com/envoyproxy/gateway/proto/extension"
	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	"github.com/wtzhang23/xds-backend/pkg/types"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type handlerKey struct {
	kind       string
	apiVersion string
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

func (s *Server) PostClusterModify(ctx context.Context, req *pb.PostClusterModifyRequest) (*pb.PostClusterModifyResponse, error) {
	cluster := req.Cluster
	s.log.DebugContext(ctx, "PostClusterModify callback invoked", slog.String("cluster", cluster.Name))
	for _, ext := range req.PostClusterContext.BackendExtensionResources {
		var resourceInfo map[string]any
		if err := json.Unmarshal(ext.GetUnstructuredBytes(), &resourceInfo); err != nil {
			return nil, err
		}
		kind, ok := resourceInfo["kind"].(string)
		if !ok {
			return nil, status.Error(codes.InvalidArgument, "kind is required")
		}
		apiVersion, ok := resourceInfo["apiVersion"].(string)
		if !ok {
			return nil, status.Error(codes.InvalidArgument, "apiVersion is required")
		}
		key := handlerKey{kind: kind, apiVersion: apiVersion}
		handler, ok := s.handlers[key]
		if !ok {
			s.log.DebugContext(ctx, "Handler not found", slog.String("kind", kind), slog.String("apiVersion", apiVersion))
			continue
		}
		s.log.DebugContext(ctx, "Handler found", slog.String("kind", kind), slog.String("apiVersion", apiVersion))
		backendConfig, err := handler.ParseBackendFromBytes(ext.GetUnstructuredBytes())
		if err != nil {
			return nil, status.Errorf(codes.Internal, "failed to parse backend config from bytes: %v", err)
		}
		s.changeXdsOfCluster(ctx, cluster, backendConfig)
	}
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
