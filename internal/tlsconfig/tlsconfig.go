// Package tlsconfig provides TLS configuration utilities for gRPC servers.
package tlsconfig

import (
	"crypto/tls"
	"log/slog"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

// Config holds TLS configuration for gRPC servers.
type Config struct {
	CertFile string
	KeyFile  string
}

// AddTLSCredentials adds TLS credentials to gRPC server options if TLS is configured.
// If TLS configuration is provided and valid, it adds TLS credentials to the options.
// If there's an error loading the certificate, it logs the error and returns options without TLS.
// Returns the modified options slice.
func AddTLSCredentials(opts []grpc.ServerOption, tlsConfig *Config, logger *slog.Logger) []grpc.ServerOption {
	if tlsConfig == nil || tlsConfig.CertFile == "" || tlsConfig.KeyFile == "" {
		return opts
	}

	cert, err := tls.LoadX509KeyPair(tlsConfig.CertFile, tlsConfig.KeyFile)
	if err != nil {
		logger.Error("Failed to load TLS certificate", slog.String("error", err.Error()))
		// Return options without TLS rather than failing completely
		return opts
	}

	tlsServerConfig := &tls.Config{
		Certificates: []tls.Certificate{cert},
	}
	opts = append(opts, grpc.Creds(credentials.NewTLS(tlsServerConfig)))
	logger.Info("TLS enabled", slog.String("cert-file", tlsConfig.CertFile), slog.String("key-file", tlsConfig.KeyFile))

	return opts
}

