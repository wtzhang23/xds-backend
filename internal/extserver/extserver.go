// extserver is a package that starts the extension server.
package extserver

import (
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"

	pb "github.com/envoyproxy/gateway/proto/extension"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/wtzhang23/xds-backend/internal/handler"
	"github.com/wtzhang23/xds-backend/internal/interceptors"
	"github.com/wtzhang23/xds-backend/internal/tlsconfig"
	"github.com/wtzhang23/xds-backend/pkg/server"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	"google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/reflection"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

var grpcServer *grpc.Server
var grpcTLSServer *grpc.Server
var httpServer *http.Server

func StartExtensionServer(
	host string,
	grpcPort int,
	httpPort int,
	metricsPort int,
	logLevel slog.Level,
	tlsConfig *tlsconfig.Config,
	tlsPort int,
) error {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: logLevel,
	}))

	var wg sync.WaitGroup

	// Start HTTP server
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := startHTTPServer(net.JoinHostPort(host, strconv.Itoa(httpPort)), logger); err != nil {
			logger.Error("HTTP server error", slog.String("error", err.Error()))
		}
	}()

	// Start metrics server
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := startMetricsServer(net.JoinHostPort(host, strconv.Itoa(metricsPort)), logger); err != nil {
			logger.Error("Metrics server error", slog.String("error", err.Error()))
		}
	}()

	// Create a single extension server instance to be shared by both gRPC servers
	extensionServer := server.NewServer(
		logger,
		&handler.XdsBackendHandler{},
	)

	// Create and start plain gRPC server if port is provided (non-zero)
	if grpcPort > 0 {
		plainServer := createGRPCServer(logger, nil, extensionServer)
		grpcServer = plainServer

		// Start plain gRPC server on the first port
		plainLis, err := net.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(grpcPort)))
		if err != nil {
			return err
		}
		logger.Info("Starting gRPC extension server", slog.String("grpc-address", plainLis.Addr().String()))
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := plainServer.Serve(plainLis); err != nil {
				logger.Error("gRPC server error", slog.String("error", err.Error()))
			}
		}()
	}

	// If TLS is configured, start a separate TLS server on the TLS port
	// Note: We need a separate server because TLS is configured at server creation time.
	// A single server instance can serve multiple listeners, but TLS is all-or-nothing
	// at the server level. To have one plain and one TLS listener, we need two servers.
	// Both servers share the same extension server instance.
	if tlsConfig != nil && tlsConfig.CertFile != "" && tlsConfig.KeyFile != "" {
		tlsServer := createGRPCServer(logger, tlsConfig, extensionServer)
		grpcTLSServer = tlsServer
		tlsLis, err := net.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(tlsPort)))
		if err != nil {
			return err
		}
		logger.Info("Starting TLS gRPC extension server", slog.String("grpc-address", tlsLis.Addr().String()))
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := tlsServer.Serve(tlsLis); err != nil {
				logger.Error("TLS gRPC server error", slog.String("error", err.Error()))
			}
		}()
	}

	// Wait for all servers to finish (they run indefinitely until stopped)
	wg.Wait()
	return nil
}

func createGRPCServer(logger *slog.Logger, tlsConfig *tlsconfig.Config, extensionServer *server.Server) *grpc.Server {
	var opts []grpc.ServerOption
	opts = append(opts, grpc.ChainUnaryInterceptor(
		metricsUnaryInterceptor(),
		interceptors.LoggingUnaryInterceptor(logger),
	))
	opts = append(opts, grpc.ChainStreamInterceptor(
		interceptors.LoggingStreamInterceptor(logger),
	))

	// Add TLS credentials if configured
	// Note: TLS is configured at server creation time and applies to all listeners
	// on this server instance. To have both plain and TLS listeners, use separate servers.
	opts = tlsconfig.AddTLSCredentials(opts, tlsConfig, logger)

	grpcServer := grpc.NewServer(opts...)

	// Register all services on the gRPC server
	registerServices(grpcServer, extensionServer)

	return grpcServer
}

// registerServices registers all services (extension server, reflection, health check) on the gRPC server
func registerServices(grpcServer *grpc.Server, extensionServer *server.Server) {
	// Register the extension server
	pb.RegisterEnvoyGatewayExtensionServer(grpcServer, extensionServer)

	// Register reflection service for gRPC CLI tools
	reflection.Register(grpcServer)

	// Register health check service
	healthServer := health.NewServer()
	grpc_health_v1.RegisterHealthServer(grpcServer, healthServer)
	healthServer.SetServingStatus("", grpc_health_v1.HealthCheckResponse_SERVING)
}

func startHTTPServer(httpAddress string, log *slog.Logger) error {
	log.Info("Starting HTTP server", slog.String("http-address", httpAddress))
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		log.Debug("Healthz request received")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("OK"))
	})
	httpServer = &http.Server{
		Addr:    httpAddress,
		Handler: mux,
	}
	return httpServer.ListenAndServe()
}

func startMetricsServer(metricsAddress string, log *slog.Logger) error {
	log.Info("Starting metrics server", slog.String("metrics-address", metricsAddress))
	mux := http.NewServeMux()
	// Use controller-runtime metrics registry
	mux.Handle("/metrics", promhttp.HandlerFor(metrics.Registry, promhttp.HandlerOpts{}))
	metricsServer := &http.Server{
		Addr:    metricsAddress,
		Handler: mux,
	}
	return metricsServer.ListenAndServe()
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
