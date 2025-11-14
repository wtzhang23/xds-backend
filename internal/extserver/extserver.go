// extserver is a package that starts the extension server.
package extserver

import (
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	pb "github.com/envoyproxy/gateway/proto/extension"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/wtzhang23/xds-backend/internal/handler"
	"github.com/wtzhang23/xds-backend/pkg/server"
	"google.golang.org/grpc"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

var grpcServer *grpc.Server
var httpServer *http.Server

func StartExtensionServer(
	host string,
	grpcPort int,
	httpPort int,
	metricsPort int,
	logLevel slog.Level,
) error {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: logLevel,
	}))
	go startHTTPServer(net.JoinHostPort(host, strconv.Itoa(httpPort)), logger)
	go startMetricsServer(net.JoinHostPort(host, strconv.Itoa(metricsPort)), logger)
	return startGRPCServer(net.JoinHostPort(host, strconv.Itoa(grpcPort)), logger)
}

func startGRPCServer(grpcAddress string, logger *slog.Logger) error {
	logger.Info("Starting GRPC extension server", slog.String("grpc-address", grpcAddress))
	lis, err := net.Listen("tcp", grpcAddress)
	if err != nil {
		return err
	}
	var opts []grpc.ServerOption
	opts = append(opts, grpc.ChainUnaryInterceptor(
		metricsUnaryInterceptor(),
		loggingUnaryInterceptor(logger),
	))
	grpcServer = grpc.NewServer(opts...)
	extensionServer := server.NewServer(
		logger,
		&handler.XdsBackendHandler{},
	)
	pb.RegisterEnvoyGatewayExtensionServer(grpcServer, extensionServer)
	return grpcServer.Serve(lis)
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
				os.Exit(0)
			}
		}
	}()
	return nil
}
