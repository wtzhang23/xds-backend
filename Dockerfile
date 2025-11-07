FROM golang:1.25.3 AS builder

ARG GO_LDFLAGS=""

WORKDIR /workspace
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/root/.cache/go-build --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . ./
RUN --mount=type=cache,target=/root/.cache/go-build --mount=type=cache,target=/go/pkg/mod \
    CGO_ENABLED=0  \
    GOOS=${TARGETOS} \
    GOARCH=${TARGETARCH} \
    go build -o /bin/xds-backend-extension-server -ldflags "${GO_LDFLAGS}" cmd/xds-backend-extension-server/main.go

FROM gcr.io/distroless/static-debian11
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=builder /bin/xds-backend-extension-server /

ENTRYPOINT ["/xds-backend-extension-server", "server"]