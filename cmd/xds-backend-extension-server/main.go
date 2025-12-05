package main

import (
	"log/slog"
	"os"

	"github.com/urfave/cli"
	"github.com/wtzhang23/xds-backend/internal/extserver"
	"github.com/wtzhang23/xds-backend/internal/filexds"
	"github.com/wtzhang23/xds-backend/internal/tlsconfig"
)

func main() {
	app := &cli.App{
		Name:  "xds-backend-extension-server",
		Usage: "A server for the xDS backend extension",
		Commands: []cli.Command{
			{
				Name:  "server",
				Usage: "runs the Extension Server",
				Before: func(_ *cli.Context) error {
					return extserver.HandleSignals()
				},
				Action: func(ctx *cli.Context) error {
					var level slog.Level
					if err := level.UnmarshalText([]byte(ctx.String("log-level"))); err != nil {
						level = slog.LevelInfo
					}
					var tlsConfig *tlsconfig.Config
					if ctx.String("tls-cert-file") != "" && ctx.String("tls-key-file") != "" {
						tlsConfig = &tlsconfig.Config{
							CertFile: ctx.String("tls-cert-file"),
							KeyFile:  ctx.String("tls-key-file"),
						}
					}
					cfg := &extserver.ExtensionServerConfig{
						Host:        ctx.String("host"),
						GrpcPort:    ctx.Int("grpc-port"),
						HttpPort:    ctx.Int("http-port"),
						MetricsPort: ctx.Int("metrics-port"),
						LogLevel:    level,
						TlsConfig:   tlsConfig,
						TlsPort:     ctx.Int("tls-port"),
						UdsPath:     ctx.String("grpc-uds-path"),
					}
					return extserver.StartExtensionServer(cfg)
				},
				Flags: []cli.Flag{
					&cli.StringFlag{
						Name:  "host",
						Usage: "the host on which to listen",
						Value: "0.0.0.0",
					},
					&cli.IntFlag{
						Name:  "grpc-port",
						Usage: "the port on which to listen for gRPC requests (TCP). Must be explicitly specified (non-zero) to enable TCP listener. Can be used alongside grpc-uds-path.",
						Value: 0,
					},
					&cli.StringFlag{
						Name:  "grpc-uds-path",
						Usage: "Unix Domain Socket path on which to listen for gRPC requests. Can be used alongside grpc-port to serve both TCP and UDS simultaneously.",
						Value: "",
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
					&cli.StringFlag{
						Name:  "tls-cert-file",
						Usage: "path to TLS certificate file (enables TLS when provided with tls-key-file)",
						Value: "",
					},
					&cli.StringFlag{
						Name:  "tls-key-file",
						Usage: "path to TLS private key file (enables TLS when provided with tls-cert-file)",
						Value: "",
					},
					&cli.IntFlag{
						Name:  "tls-port",
						Usage: "the port on which to listen for TLS gRPC requests",
						Value: 5006,
					},
				},
			},
			{
				Name:  "filexds",
				Usage: "runs the File xDS server",
				Action: func(ctx *cli.Context) error {
					var level slog.Level
					if err := level.UnmarshalText([]byte(ctx.String("log-level"))); err != nil {
						level = slog.LevelInfo
					}
					logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
						Level: level,
					}))
					var tlsConfig *tlsconfig.Config
					if ctx.String("tls-cert-file") != "" && ctx.String("tls-key-file") != "" {
						tlsConfig = &tlsconfig.Config{
							CertFile: ctx.String("tls-cert-file"),
							KeyFile:  ctx.String("tls-key-file"),
						}
					}
					return filexds.StartXdsServer(
						ctx.String("host"),
						ctx.Int("grpc-port"),
						ctx.String("file-path"),
						logger,
						tlsConfig,
						ctx.Int("tls-port"),
					)
				},
				Flags: []cli.Flag{
					&cli.StringFlag{
						Name:  "log-level",
						Usage: "the log level, should be one of Debug/Info/Warn/Error",
						Value: slog.LevelInfo.String(),
					},
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
					&cli.StringFlag{
						Name:  "id",
						Usage: "the id of the xDS server",
						Value: "",
					},
					&cli.StringFlag{
						Name:  "file-path",
						Usage: "the path to the file to watch for changes",
						Value: "/etc/envoy/eds.yaml",
					},
					&cli.StringFlag{
						Name:  "tls-cert-file",
						Usage: "path to TLS certificate file (enables TLS when provided with tls-key-file)",
						Value: "",
					},
					&cli.StringFlag{
						Name:  "tls-key-file",
						Usage: "path to TLS private key file (enables TLS when provided with tls-cert-file)",
						Value: "",
					},
					&cli.IntFlag{
						Name:  "tls-port",
						Usage: "the port on which to listen for TLS gRPC requests",
						Value: 5006,
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
