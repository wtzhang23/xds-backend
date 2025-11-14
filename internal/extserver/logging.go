package extserver

import (
	"context"
	"log/slog"

	"google.golang.org/grpc"
)

// loggingUnaryInterceptor returns a gRPC unary interceptor that logs errors
func loggingUnaryInterceptor(logger *slog.Logger) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
		resp, err := handler(ctx, req)
		if err != nil {
			logger.Error("Error handling request", slog.String("method", info.FullMethod), slog.String("error", err.Error()))
		}
		return resp, err
	}
}
