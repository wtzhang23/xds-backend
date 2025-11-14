// fileeds is a package that sources endpoints from a local file and advertises them as an EDS server.
package fileeds

import (
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	_ "github.com/envoyproxy/go-control-plane/envoy/config/endpoint/v3"
	endpointv3 "github.com/envoyproxy/go-control-plane/envoy/config/endpoint/v3"
	discoveryv3 "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
	endpointservice "github.com/envoyproxy/go-control-plane/envoy/service/endpoint/v3"
	"github.com/envoyproxy/go-control-plane/pkg/cache/types"
	cachev3 "github.com/envoyproxy/go-control-plane/pkg/cache/v3"
	resourcev3 "github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	xdsv3 "github.com/envoyproxy/go-control-plane/pkg/server/v3"
	"github.com/fsnotify/fsnotify"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/encoding/protojson"
	"gopkg.in/yaml.v2"
)

const catchAllHash = "catch-all-hash"

var grpcServer *grpc.Server

func StartEdsServer(host string, port int, filePath string, logger *slog.Logger) error {
	address := net.JoinHostPort(host, strconv.Itoa(port))
	logger.Info("Starting EDS server", slog.String("address", address), slog.String("file_path", filePath))
	lis, err := net.Listen("tcp", address)
	if err != nil {
		return err
	}
	snapshotCache, err := createAndStartCache(filePath, logger)
	if err != nil {
		return err
	}
	server := xdsv3.NewServer(context.Background(), snapshotCache, nil)
	grpcServer := grpc.NewServer(grpc.ChainUnaryInterceptor(
		loggingUnaryInterceptor(logger),
	))
	endpointservice.RegisterEndpointDiscoveryServiceServer(grpcServer, server)

	return grpcServer.Serve(lis)
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

			edsResources := make(map[resourcev3.Type][]types.Resource)
			for _, resource := range response.Resources {
				var eds endpointv3.ClusterLoadAssignment
				err := resource.UnmarshalTo(&eds)
				if err != nil {
					logger.Error("Error unmarshalling EDS resource", slog.String("error", err.Error()))
					continue
				}
				edsResources[resourcev3.EndpointType] = []types.Resource{&eds}
			}
			snapshot, err := cachev3.NewSnapshot(response.VersionInfo, edsResources)
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
				os.Exit(0)
			}
		}
	}()
	return nil
}

var _ cachev3.NodeHash = CatchAllHash{}

type CatchAllHash struct{}

func (h CatchAllHash) ID(node *corev3.Node) string {
	return catchAllHash

}
