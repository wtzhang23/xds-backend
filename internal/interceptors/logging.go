package interceptors

import (
	"context"
	"io"
	"log/slog"

	"google.golang.org/grpc"
)

// LoggingUnaryInterceptor returns a gRPC unary interceptor that logs requests and errors
func LoggingUnaryInterceptor(logger *slog.Logger) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
		logger.Debug("Handling unary request", slog.String("method", info.FullMethod))
		resp, err := handler(ctx, req)
		if err != nil {
			logger.Error("Error handling unary request", slog.String("method", info.FullMethod), slog.String("error", err.Error()))
		}
		return resp, err
	}
}

// LoggingStreamInterceptor returns a gRPC stream interceptor that logs streaming requests and errors
func LoggingStreamInterceptor(logger *slog.Logger) grpc.StreamServerInterceptor {
	return func(srv interface{}, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		logger.Debug("Handling stream request", slog.String("method", info.FullMethod))
		err := handler(srv, ss)
		if err != nil && err != io.EOF {
			logger.Error("Error handling stream request", slog.String("method", info.FullMethod), slog.String("error", err.Error()))
		}
		return err
	}
}
