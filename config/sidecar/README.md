# Sidecar Deployment Configuration

This directory contains configuration files and instructions for deploying the xDS Backend extension server as a sidecar container alongside the Envoy Gateway controller.

## Overview

When deployed as a sidecar, the extension server runs in the same pod as the Envoy Gateway controller, enabling:
- Lower latency communication via Unix Domain Socket (UDS)
- Simplified networking (no Service required)
- Co-located lifecycle management

## Prerequisites

1. Envoy Gateway must be installed via Helm
2. kubectl configured to access your Kubernetes cluster

## Deployment Steps

### 1. Install Envoy Gateway with Sidecar Configuration

Install Envoy Gateway using Helm with the sidecar values:

```bash
helm install eg oci://docker.io/envoyproxy/gateway-helm \
  --namespace envoy-gateway-system \
  --create-namespace \
  -f config/sidecar/values-sidecar.yaml
```

### 2. Patch Envoy Gateway Deployment

Patch the Envoy Gateway deployment to add the extension server sidecar container:

```bash
kubectl patch deployment envoy-gateway -n envoy-gateway-system --type='json' -p='
[
  {
    "op": "add",
    "path": "/spec/template/spec/containers/-",
    "value": {
      "name": "xds-backend-extension-server",
      "image": "wtzhang23/xds-backend-extension-server:latest",
      "imagePullPolicy": "IfNotPresent",
      "args": [
        "server",
        "--log-level",
        "info",
        "--grpc-uds-path",
        "/tmp/xds-backend.sock",
        "--metrics-port",
        "8081",
        "--http-port",
        "8080"
      ],
      "ports": [
        {
          "name": "http",
          "containerPort": 8080,
          "protocol": "TCP"
        },
        {
          "name": "metrics",
          "containerPort": 8081,
          "protocol": "TCP"
        }
      ],
      "volumeMounts": [
        {
          "name": "extension-server-socket",
          "mountPath": "/tmp"
        }
      ],
      "resources": {
        "requests": {
          "cpu": "100m",
          "memory": "128Mi"
        },
        "limits": {
          "cpu": "500m",
          "memory": "256Mi"
        }
      },
      "livenessProbe": {
        "httpGet": {
          "path": "/healthz",
          "port": "http"
        },
        "initialDelaySeconds": 10,
        "periodSeconds": 10
      },
      "readinessProbe": {
        "httpGet": {
          "path": "/healthz",
          "port": "http"
        },
        "initialDelaySeconds": 5,
        "periodSeconds": 5
      }
    }
  },
  {
    "op": "add",
    "path": "/spec/template/spec/volumes/-",
    "value": {
      "name": "extension-server-socket",
      "emptyDir": {}
    }
  }
]'
```

Alternatively, you can use `kubectl edit` to manually add the sidecar container:

```bash
kubectl edit deployment envoy-gateway -n envoy-gateway-system
```

Then add the sidecar container to the `spec.template.spec.containers` array and the volume to `spec.template.spec.volumes`.

## Configuration Details

### Sidecar Container

The sidecar container is configured with:
- **Image**: `wtzhang23/xds-backend-extension-server:latest` (customize as needed)
- **UDS Path**: `/tmp/xds-backend.sock` (shared via emptyDir volume)
- **HTTP Port**: 8080 (for health checks)
- **Metrics Port**: 8081 (for Prometheus metrics)

### Volume Sharing

The sidecar and Envoy Gateway controller share an `emptyDir` volume mounted at `/tmp` to enable UDS communication.

### Envoy Gateway Configuration

The Envoy Gateway is configured to connect to the extension server via UDS at `/tmp/xds-backend.sock`.

**Note**: If Envoy Gateway doesn't support UDS configuration yet, you may need to use `localhost:5005` instead and configure the sidecar to listen on TCP port 5005.

## Customization

### Update Image

When patching the deployment, change the `image` field in the sidecar container specification:

```json
{
  "name": "xds-backend-extension-server",
  "image": "your-registry/xds-backend-extension-server:your-tag",
  ...
}
```

### Update UDS Path

To change the UDS path, update both:
1. The sidecar container's `--grpc-uds-path` argument in the deployment patch
2. `values-sidecar.yaml` - Envoy Gateway `service.unix.path` configuration

### Add TLS Support

To enable TLS for the sidecar, add TLS certificate volume mounts and update the sidecar args:

```yaml
args:
  - server
  - --tls-cert-file
  - /etc/tls/tls.crt
  - --tls-key-file
  - /etc/tls/tls.key
  - --tls-port
  - "5006"
```

## Verification

After deployment, verify the sidecar is running:

```bash
kubectl get pods -n envoy-gateway-system
kubectl logs -n envoy-gateway-system deployment/envoy-gateway -c xds-backend-extension-server
```

Check that Envoy Gateway can connect to the extension server:

```bash
kubectl logs -n envoy-gateway-system deployment/envoy-gateway -c envoy-gateway
```

## Troubleshooting

### Sidecar Not Starting

- Check pod events: `kubectl describe pod -n envoy-gateway-system -l app.kubernetes.io/name=gateway-helm`
- Check sidecar logs: `kubectl logs -n envoy-gateway-system <pod-name> -c xds-backend-extension-server`

### Connection Issues

- Verify the UDS socket exists: `kubectl exec -n envoy-gateway-system <pod-name> -c envoy-gateway -- ls -la /tmp/xds-backend.sock`
- Check Envoy Gateway logs for connection errors

### Image Pull Errors

- Ensure the extension server image is available in your cluster
- Update `imagePullPolicy` if using a private registry

