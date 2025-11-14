package main

import (
	"log/slog"
	"os"

	"github.com/urfave/cli"
	"github.com/wtzhang23/xds-backend/internal/extserver"
	"github.com/wtzhang23/xds-backend/internal/fileeds"
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
					return extserver.StartExtensionServer(
						ctx.String("host"),
						ctx.Int("grpc-port"),
						ctx.Int("http-port"),
						ctx.Int("metrics-port"),
						level,
					)
				},
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
			{
				Name:  "fileeds",
				Usage: "runs the File EDS server",
				Action: func(ctx *cli.Context) error {
					var level slog.Level
					if err := level.UnmarshalText([]byte(ctx.String("log-level"))); err != nil {
						level = slog.LevelInfo
					}
					logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
						Level: level,
					}))
					return fileeds.StartEdsServer(
						ctx.String("host"),
						ctx.Int("grpc-port"),
						ctx.String("file-path"),
						logger,
					)
				},
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
					&cli.StringFlag{
						Name:  "id",
						Usage: "the id of the EDS server",
						Value: "",
					},
					&cli.StringFlag{
						Name:  "file-path",
						Usage: "the path to the file to watch for changes",
						Value: "/etc/envoy/eds.yaml",
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
