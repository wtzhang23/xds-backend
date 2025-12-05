// extserver is a package that starts the extension server.
package extserver

import (
	"fmt"
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
var grpcUdsPath string

type ExtensionServerConfig struct {
	Host        string
	GrpcPort    int
	HttpPort    int
	TlsConfig   *tlsconfig.Config
	TlsPort     int
	UdsPath     string
	MetricsPort int
	LogLevel    slog.Level
}

func StartExtensionServer(
	cfg *ExtensionServerConfig,
) error {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: cfg.LogLevel,
	}))

	var wg sync.WaitGroup

	// Start HTTP server
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := startHTTPServer(net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.HttpPort)), logger); err != nil {
			logger.Error("HTTP server error", slog.String("error", err.Error()))
		}
	}()

	// Start metrics server
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := startMetricsServer(net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.MetricsPort)), logger); err != nil {
			logger.Error("Metrics server error", slog.String("error", err.Error()))
		}
	}()

	// Create a single extension server instance to be shared by both gRPC servers
	extensionServer := server.NewServer(
		logger,
		&handler.XdsBackendHandler{},
	)

	// Create a shared gRPC server instance for both TCP and UDS listeners
	// At least one of TCP (grpcPort > 0) or UDS (udsPath != "") must be configured
	if cfg.GrpcPort <= 0 && cfg.UdsPath == "" {
		return fmt.Errorf("at least one of grpc-port (must be > 0) or grpc-uds-path must be specified")
	}

	var plainServer *grpc.Server
	if cfg.GrpcPort > 0 || cfg.UdsPath != "" {
		plainServer = createGRPCServer(logger, nil, extensionServer)
		grpcServer = plainServer
	}

	// Start TCP listener if port is explicitly provided (non-zero)
	if cfg.GrpcPort > 0 {
		tcpLis, err := net.Listen("tcp", net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.GrpcPort)))
		if err != nil {
			return err
		}
		logger.Info("Starting gRPC extension server (TCP)", slog.String("grpc-address", tcpLis.Addr().String()))
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := plainServer.Serve(tcpLis); err != nil {
				logger.Error("gRPC TCP server error", slog.String("error", err.Error()))
			}
		}()
	}

	// Start UDS listener if path is provided (can run alongside TCP)
	if cfg.UdsPath != "" {
		// Store UDS path for cleanup on shutdown
		grpcUdsPath = cfg.UdsPath
		// Remove existing socket file if it exists
		if _, err := os.Stat(cfg.UdsPath); err == nil {
			if err := os.Remove(cfg.UdsPath); err != nil {
				return err
			}
		}
		udsLis, err := net.Listen("unix", cfg.UdsPath)
		if err != nil {
			return err
		}
		logger.Info("Starting gRPC extension server (UDS)", slog.String("grpc-address", udsLis.Addr().String()))
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := plainServer.Serve(udsLis); err != nil {
				logger.Error("gRPC UDS server error", slog.String("error", err.Error()))
			}
			// Clean up UDS socket file on shutdown
			if grpcUdsPath != "" {
				os.Remove(grpcUdsPath)
			}
		}()
	}

	// If TLS is configured, start a separate TLS server on the TLS port
	// Note: We need a separate server because TLS is configured at server creation time.
	// A single server instance can serve multiple listeners, but TLS is all-or-nothing
	// at the server level. To have one plain and one TLS listener, we need two servers.
	// Both servers share the same extension server instance.
	if cfg.TlsConfig != nil && cfg.TlsConfig.CertFile != "" && cfg.TlsConfig.KeyFile != "" {
		tlsServer := createGRPCServer(logger, cfg.TlsConfig, extensionServer)
		grpcTLSServer = tlsServer
		tlsLis, err := net.Listen("tcp", net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.TlsPort)))
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

// registerServices registers all services (extension server, health check, reflection) on the gRPC server
// Reflection is registered last so it can discover all other services
func registerServices(grpcServer *grpc.Server, extensionServer *server.Server) {
	// Register the extension server
	pb.RegisterEnvoyGatewayExtensionServer(grpcServer, extensionServer)

	// Register health check service
	healthServer := health.NewServer()
	grpc_health_v1.RegisterHealthServer(grpcServer, healthServer)
	healthServer.SetServingStatus("", grpc_health_v1.HealthCheckResponse_SERVING)

	// Register reflection service for gRPC CLI tools (last, so it can discover all services)
	reflection.Register(grpcServer)
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
			// Clean up UDS socket file on shutdown
			if grpcUdsPath != "" {
				os.Remove(grpcUdsPath)
			}
			os.Exit(0)
		}
	}()
	return nil
}
