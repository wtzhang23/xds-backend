package main

import (
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	pb "github.com/envoyproxy/gateway/proto/extension"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/urfave/cli"
	"github.com/wtzhang23/xds-backend/internal/handler"
	"github.com/wtzhang23/xds-backend/pkg/server"
	"google.golang.org/grpc"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

func main() {
	app := &cli.App{
		Name:  "xds-backend-extension-server",
		Usage: "A server for the xDS backend extension",
		Commands: []cli.Command{
			{
				Name:   "server",
				Usage:  "runs the Extension Server",
				Before: handleSignals,
				Action: startExtensionServer,
				Flags: []cli.Flag{
					&cli.StringFlag{
						Name:  "host",
						Usage: "the host on which to listen",
						Value: "0.0.0.0",
					},
					&cli.IntFlag{
						Name:  "grpc-port",
						Usage: "the port on which to listen for gRPC requests",
						Value: 5005,
					},
					&cli.IntFlag{
						Name:  "http-port",
						Usage: "the port on which to listen for HTTP requests",
						Value: 8080,
					},
					&cli.IntFlag{
						Name:  "metrics-port",
						Usage: "the port on which to expose metrics",
						Value: 8081,
					},
					&cli.StringFlag{
						Name:  "log-level",
						Usage: "the log level, should be one of Debug/Info/Warn/Error",
						Value: slog.LevelInfo.String(),
					},
				},
			},
		},
	}
	if err := app.Run(os.Args); err != nil {
		slog.Error("Failed to run the application", slog.String("error", err.Error()))
		os.Exit(1)
	}
}

var grpcServer *grpc.Server
var httpServer *http.Server

func handleSignals(cCtx *cli.Context) error {
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

func startExtensionServer(cCtx *cli.Context) error {
	var level slog.Level
	if err := level.UnmarshalText([]byte(cCtx.String("log-level"))); err != nil {
		level = slog.LevelInfo
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: level,
	}))
	grpcAddress := net.JoinHostPort(cCtx.String("host"), cCtx.String("grpc-port"))
	logger.Info("Starting the extension server", slog.String("grpc-address", grpcAddress))
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
	go startHTTPServer(cCtx, logger)
	go startMetricsServer(cCtx, logger)
	return grpcServer.Serve(lis)
}

func startHTTPServer(cCtx *cli.Context, log *slog.Logger) error {
	httpAddress := net.JoinHostPort(cCtx.String("host"), cCtx.String("http-port"))
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

func startMetricsServer(cCtx *cli.Context, log *slog.Logger) error {
	metricsAddress := net.JoinHostPort(cCtx.String("host"), cCtx.String("metrics-port"))
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
