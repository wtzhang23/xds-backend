package main

import (
	"context"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

var (
	grpcRequestsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "grpc_requests_total",
			Help: "Total number of gRPC requests",
		},
		[]string{"method", "code"},
	)
	grpcRequestDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "grpc_request_duration_seconds",
			Help:    "gRPC request duration in seconds",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"method", "code"},
	)
)

func init() {
	// Register gRPC metrics with controller-runtime metrics registry
	metrics.Registry.MustRegister(grpcRequestsTotal)
	metrics.Registry.MustRegister(grpcRequestDuration)
}

// metricsUnaryInterceptor returns a gRPC unary interceptor that collects metrics
func metricsUnaryInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
		start := time.Now()
		resp, err := handler(ctx, req)
		duration := time.Since(start).Seconds()

		// Get status code from error
		code := codes.OK.String()
		if err != nil {
			if st, ok := status.FromError(err); ok {
				code = st.Code().String()
			} else {
				code = codes.Unknown.String()
			}
		}

		// Record metrics
		method := info.FullMethod
		grpcRequestsTotal.WithLabelValues(method, code).Inc()
		grpcRequestDuration.WithLabelValues(method, code).Observe(duration)

		return resp, err
	}
}
