# E2E Tests

This directory contains end-to-end tests for the xds-backend extension server.

## Prerequisites

The e2e tests require the following tools to be installed and available in your PATH:

- [kind](https://kind.sigs.k8s.io/) - Kubernetes in Docker
- [kubectl](https://kubernetes.io/docs/tasks/tools/) - Kubernetes command-line tool
- [helm](https://helm.sh/) - Kubernetes package manager
- [docker](https://www.docker.com/) - Container runtime
- [go](https://go.dev/) - Go programming language

## Running the Tests

To run the e2e tests:

```bash
# From the project root
go test -v ./test/e2e/...
```

Or using the Makefile target:

```bash
make test-e2e
```

## Test Structure

The e2e tests are organized as follows:

- `e2e_test.go` - Main test file with Ginkgo test suite setup
- `kind.go` - Utilities for managing kind clusters
- `envoygateway.go` - Utilities for installing and managing Envoy Gateway
- `extension_server.go` - Utilities for deploying the extension server
- `constants.go` - Shared constants used across test files
- `utils.go` - Utility functions

## Test Flow

1. **Setup** (BeforeSuite):
   - Creates a kind cluster named `xds-backend-e2e`
   - Installs Envoy Gateway using Helm
   - Builds and deploys the extension server

2. **Tests**:
   - Individual test cases can be added here

3. **Teardown** (AfterSuite):
   - Cleans up the kind cluster

## Skipping Tests

To skip e2e tests (e.g., in CI environments where kind is not available):

```bash
SKIP_E2E=true go test ./test/e2e/...
```

## Configuration

The tests use the following default values:

- Cluster name: `xds-backend-e2e`
- Envoy Gateway namespace: `envoy-gateway-system`
- Extension server namespace: `xds-backend-system`
- Test timeout: 5 minutes
- Poll interval: 2 seconds

These can be modified in `constants.go` if needed.

## Troubleshooting

### Cluster already exists

If a cluster with the same name already exists, the test will attempt to use it. To start fresh:

```bash
kind delete cluster --name xds-backend-e2e
```

### Image build failures

Ensure Docker is running and you have permissions to build images.

### Helm installation failures

Ensure the Envoy Gateway Helm repository is accessible and Helm is properly configured.

