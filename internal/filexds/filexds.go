// filexds is a package that sources xDS resources from a local file and advertises them as an xDS server.
package filexds

import (
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"

	_ "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	_ "github.com/envoyproxy/go-control-plane/envoy/config/endpoint/v3"
	_ "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	_ "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	_ "github.com/envoyproxy/go-control-plane/envoy/extensions/transport_sockets/tls/v3"

	clusterservice "github.com/envoyproxy/go-control-plane/envoy/service/cluster/v3"
	discoveryv3 "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
	endpointservice "github.com/envoyproxy/go-control-plane/envoy/service/endpoint/v3"
	extensionservice "github.com/envoyproxy/go-control-plane/envoy/service/extension/v3"
	listenerservice "github.com/envoyproxy/go-control-plane/envoy/service/listener/v3"
	routeservice "github.com/envoyproxy/go-control-plane/envoy/service/route/v3"
	secretservice "github.com/envoyproxy/go-control-plane/envoy/service/secret/v3"
	"github.com/envoyproxy/go-control-plane/pkg/cache/types"
	cachev3 "github.com/envoyproxy/go-control-plane/pkg/cache/v3"
	resourcev3 "github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	xdsv3 "github.com/envoyproxy/go-control-plane/pkg/server/v3"
	"github.com/fsnotify/fsnotify"
	"github.com/wtzhang23/xds-backend/internal/interceptors"
	"github.com/wtzhang23/xds-backend/internal/tlsconfig"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	"google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/reflection"
	"google.golang.org/protobuf/encoding/protojson"
	"gopkg.in/yaml.v3"
)

const catchAllHash = "catch-all-hash"

var grpcServer *grpc.Server
var grpcTLSServer *grpc.Server

func StartXdsServer(host string, port int, filePath string, logger *slog.Logger, tlsConfig *tlsconfig.Config, tlsPort int) error {
	snapshotCache, err := createAndStartCache(filePath, logger)
	if err != nil {
		return err
	}

	var wg sync.WaitGroup

	// Create plain gRPC server
	plainServer := createXdsGRPCServer(snapshotCache, logger, nil)
	grpcServer = plainServer

	// Start plain gRPC server
	plainLis, err := net.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		return err
	}
	logger.Info("Starting EDS server", slog.String("address", plainLis.Addr().String()))
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := plainServer.Serve(plainLis); err != nil {
			logger.Error("Plain EDS server error", slog.String("error", err.Error()))
		}
	}()

	// Create and start TLS gRPC server if configured
	if tlsConfig != nil && tlsConfig.CertFile != "" && tlsConfig.KeyFile != "" {
		tlsServer := createXdsGRPCServer(snapshotCache, logger, tlsConfig)
		grpcTLSServer = tlsServer

		tlsLis, err := net.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(tlsPort)))
		if err != nil {
			return err
		}
		logger.Info("Starting TLS EDS server", slog.String("address", tlsLis.Addr().String()))
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := tlsServer.Serve(tlsLis); err != nil {
				logger.Error("TLS EDS server error", slog.String("error", err.Error()))
			}
		}()
	}

	// Wait for all servers to finish (they run indefinitely until stopped)
	wg.Wait()
	return nil
}

func createXdsGRPCServer(snapshotCache cachev3.Cache, logger *slog.Logger, tlsConfig *tlsconfig.Config) *grpc.Server {
	server := xdsv3.NewServer(context.Background(), snapshotCache, nil)
	var opts []grpc.ServerOption
	opts = append(opts, grpc.ChainUnaryInterceptor(
		interceptors.LoggingUnaryInterceptor(logger),
	))
	opts = append(opts, grpc.ChainStreamInterceptor(
		interceptors.LoggingStreamInterceptor(logger),
	))

	// Add TLS credentials if configured
	opts = tlsconfig.AddTLSCredentials(opts, tlsConfig, logger)

	grpcServer := grpc.NewServer(opts...)
	discoveryv3.RegisterAggregatedDiscoveryServiceServer(grpcServer, server)
	endpointservice.RegisterEndpointDiscoveryServiceServer(grpcServer, server)
	clusterservice.RegisterClusterDiscoveryServiceServer(grpcServer, server)
	routeservice.RegisterRouteDiscoveryServiceServer(grpcServer, server)
	listenerservice.RegisterListenerDiscoveryServiceServer(grpcServer, server)
	secretservice.RegisterSecretDiscoveryServiceServer(grpcServer, server)
	extensionservice.RegisterExtensionConfigDiscoveryServiceServer(grpcServer, server)

	// Register health check service
	healthServer := health.NewServer()
	grpc_health_v1.RegisterHealthServer(grpcServer, healthServer)
	healthServer.SetServingStatus("", grpc_health_v1.HealthCheckResponse_SERVING)

	// Register reflection service for gRPC CLI tools (last, so it can discover all services)
	reflection.Register(grpcServer)

	return grpcServer
}

func createAndStartCache(filePath string, logger *slog.Logger) (cachev3.Cache, error) {
	cache := cachev3.NewSnapshotCache(false, CatchAllHash{}, nil)
	go func() error {
		// Watch file for changes
		watcher, err := fsnotify.NewWatcher()
		if err != nil {
			return err
		}
		defer watcher.Close()
		watcher.Add(filePath)
		updateCache := func() error {
			content, err := os.ReadFile(filePath)
			if err != nil {
				return err
			}
			logger.Debug("Unmarshalling YAML", slog.String("content", string(content)))
			var asAny map[string]any
			err = yaml.Unmarshal(content, &asAny)
			if err != nil {
				return err
			}
			json, err := json.Marshal(asAny)
			if err != nil {
				return err
			}

			var response discoveryv3.DiscoveryResponse
			protojson.Unmarshal(json, &response)

			xdsResources := make(map[resourcev3.Type][]types.Resource)
			for _, resource := range response.Resources {
				unmarshalled, err := resource.UnmarshalNew()
				if err != nil {
					logger.Error("Error unmarshalling resource", slog.String("error", err.Error()))
					continue
				}
				xdsResources[resource.TypeUrl] = append(xdsResources[resource.TypeUrl], unmarshalled)
			}
			snapshot, err := cachev3.NewSnapshot(response.VersionInfo, xdsResources)
			if err != nil {
				return err
			}
			logger.Info("Updating cache", slog.String("version", response.VersionInfo))
			return cache.SetSnapshot(context.Background(), catchAllHash, snapshot)
		}
		if err := updateCache(); err != nil {
			logger.Error("Error updating cache", slog.String("error", err.Error()))
		}
		for {
			select {
			case _, ok := <-watcher.Events:
				if !ok {
					return nil
				}
				err := updateCache()
				if err != nil {
					logger.Error("Error updating cache", slog.String("error", err.Error()))
				}

			case err, ok := <-watcher.Errors:
				if !ok {
					return nil
				}
				logger.Error("Error watching file", slog.String("error", err.Error()))
			}
		}
	}()
	return cache, nil
}

func HandleSignals() error {
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGQUIT)
	go func() {
		for range c {
			if grpcServer != nil {
				grpcServer.Stop()
			}
			if grpcTLSServer != nil {
				grpcTLSServer.Stop()
			}
			os.Exit(0)
		}
	}()
	return nil
}

var _ cachev3.NodeHash = CatchAllHash{}

type CatchAllHash struct{}

func (h CatchAllHash) ID(node *corev3.Node) string {
	return catchAllHash

}
