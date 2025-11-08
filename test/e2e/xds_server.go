package e2e

import (
	"context"
	"fmt"
	"net"
	"sync"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	endpointv3 "github.com/envoyproxy/go-control-plane/envoy/config/endpoint/v3"
	discoveryv3 "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
	"github.com/envoyproxy/go-control-plane/pkg/cache/types"
	cachev3 "github.com/envoyproxy/go-control-plane/pkg/cache/v3"
	"github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	xdsv3 "github.com/envoyproxy/go-control-plane/pkg/server/v3"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/anypb"
)

// XdsServer represents an xDS control plane server
type XdsServer struct {
	server    xdsv3.Server
	grpc      *grpc.Server
	listener  net.Listener
	port      int
	cache     cachev3.SnapshotCache
	nodeID    string
	mu        sync.RWMutex
	endpoints map[string][]Endpoint
}

// NewXdsServer creates a new xDS server
func NewXdsServer(port int) *XdsServer {
	nodeID := "test-node"
	snapshotCache := cachev3.NewSnapshotCache(false, cachev3.IDHash{}, nil)

	return &XdsServer{
		port:      port,
		cache:     snapshotCache,
		nodeID:    nodeID,
		endpoints: make(map[string][]Endpoint),
	}
}

// Start starts the xDS server
func (x *XdsServer) Start(ctx context.Context) error {
	// Create xDS server
	cb := &callbacks{}
	x.server = xdsv3.NewServer(ctx, x.cache, cb)

	// Start gRPC server
	lis, err := net.Listen("tcp", fmt.Sprintf(":%d", x.port))
	if err != nil {
		return fmt.Errorf("failed to listen: %w", err)
	}
	x.listener = lis

	x.grpc = grpc.NewServer()
	discoveryv3.RegisterAggregatedDiscoveryServiceServer(x.grpc, x.server)

	go func() {
		if err := x.grpc.Serve(lis); err != nil {
			// Ignore error if context is cancelled
			if ctx.Err() == nil {
				fmt.Printf("xDS server error: %v\n", err)
			}
		}
	}()

	return nil
}

// SetEndpoints sets EDS endpoints for a service
func (x *XdsServer) SetEndpoints(serviceName string, endpoints []Endpoint) error {
	x.mu.Lock()
	defer x.mu.Unlock()

	x.endpoints[serviceName] = endpoints

	// Create endpoint resources
	lbEndpoints := make([]*endpointv3.LbEndpoint, 0, len(endpoints))
	for _, ep := range endpoints {
		lbEndpoints = append(lbEndpoints, &endpointv3.LbEndpoint{
			HostIdentifier: &endpointv3.LbEndpoint_Endpoint{
				Endpoint: &endpointv3.Endpoint{
					Address: &corev3.Address{
						Address: &corev3.Address_SocketAddress{
							SocketAddress: &corev3.SocketAddress{
								Protocol: corev3.SocketAddress_TCP,
								Address:  ep.Address,
								PortSpecifier: &corev3.SocketAddress_PortValue{
									PortValue: uint32(ep.Port),
								},
							},
						},
					},
				},
			},
		})
	}

	// Create cluster load assignment
	cla := &endpointv3.ClusterLoadAssignment{
		ClusterName: serviceName,
		Endpoints: []*endpointv3.LocalityLbEndpoints{
			{
				LbEndpoints: lbEndpoints,
			},
		},
	}

	// Convert to Any
	anyCLA, err := anypb.New(cla)
	if err != nil {
		return fmt.Errorf("failed to marshal cluster load assignment: %w", err)
	}

	// Create EDS resources map - using the correct resource type
	edsResources := map[resource.Type][]types.Resource{
		resource.EndpointType: {
			anyCLA,
		},
	}

	// Create new snapshot with EDS resources
	snapshot, err := cachev3.NewSnapshot("1", edsResources)
	if err != nil {
		return fmt.Errorf("failed to create snapshot: %w", err)
	}

	// Set snapshot in cache
	if err := x.cache.SetSnapshot(context.Background(), x.nodeID, snapshot); err != nil {
		return fmt.Errorf("failed to set snapshot: %w", err)
	}

	return nil
}

// Stop stops the xDS server
func (x *XdsServer) Stop() {
	if x.grpc != nil {
		x.grpc.GracefulStop()
	}
	if x.listener != nil {
		x.listener.Close()
	}
}

// GetAddress returns the address of the xDS server
func (x *XdsServer) GetAddress() string {
	return fmt.Sprintf("localhost:%d", x.port)
}

// Endpoint represents an endpoint for EDS
type Endpoint struct {
	Address string
	Port    int
}

// callbacks is a simple callback implementation for xDS server
type callbacks struct{}

func (c *callbacks) OnStreamOpen(context.Context, int64, string) error          { return nil }
func (c *callbacks) OnStreamClosed(int64, *corev3.Node)                         {}
func (c *callbacks) OnStreamRequest(int64, *discoveryv3.DiscoveryRequest) error { return nil }
func (c *callbacks) OnStreamResponse(context.Context, int64, *discoveryv3.DiscoveryRequest, *discoveryv3.DiscoveryResponse) {
}
func (c *callbacks) OnFetchRequest(context.Context, *discoveryv3.DiscoveryRequest) error           { return nil }
func (c *callbacks) OnFetchResponse(*discoveryv3.DiscoveryRequest, *discoveryv3.DiscoveryResponse) {}
func (c *callbacks) OnDeltaStreamOpen(context.Context, int64, string) error                        { return nil }
func (c *callbacks) OnDeltaStreamClosed(int64, *corev3.Node)                                       {}
func (c *callbacks) OnStreamDeltaRequest(int64, *discoveryv3.DeltaDiscoveryRequest) error          { return nil }
func (c *callbacks) OnStreamDeltaResponse(int64, *discoveryv3.DeltaDiscoveryRequest, *discoveryv3.DeltaDiscoveryResponse) {
}
