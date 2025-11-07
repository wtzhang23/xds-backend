package main

import (
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"

	pb "github.com/envoyproxy/gateway/proto/extension"
	"github.com/urfave/cli"
	"github.com/wtzhang23/xds-backend/internal/config"
	"github.com/wtzhang23/xds-backend/internal/handler"
	"github.com/wtzhang23/xds-backend/pkg/server"
	"github.com/wtzhang23/xds-backend/pkg/types"
	"github.com/wtzhang23/xds-backend/proto/v1alpha1"
	"google.golang.org/grpc"
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
						Name:  "port",
						Usage: "the port on which to listen",
						Value: 5005,
					},
					&cli.StringFlag{
						Name:  "log-level",
						Usage: "the log level, should be one of Debug/Info/Warn/Error",
						Value: slog.LevelInfo.String(),
					},
					&cli.StringFlag{
						Name:      "config-file",
						Usage:     "the path to the config file",
						Value:     "config.yaml",
						TakesFile: true,
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
	address := net.JoinHostPort(cCtx.String("host"), cCtx.String("port"))
	cfg, err := config.ParseConfig(cCtx.String("config-file"))
	if err != nil {
		return err
	}
	logger.Info("Starting the extension server", slog.String("host", address))
	lis, err := net.Listen("tcp", address)
	if err != nil {
		return err
	}
	var opts []grpc.ServerOption
	grpcServer = grpc.NewServer(opts...)
	pb.RegisterEnvoyGatewayExtensionServer(grpcServer, server.NewServer(
		logger,
		getConfigSources(cfg),
		&handler.XdsBackendHandler{},
	))
	return grpcServer.Serve(lis)
}

func getConfigSources(cfg *v1alpha1.Config) map[string]types.XdsConfigSource {
	configSources := make(map[string]types.XdsConfigSource)
	for _, source := range cfg.Sources {
		configSources[source.Name] = source
	}
	return configSources
}
