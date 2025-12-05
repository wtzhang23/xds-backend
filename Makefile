IMAGE_REPO := wtzhang23/xds-backend-extension-server
IMAGE_TAG := latest
BINARY_NAME := xds-backend-extension-server
BINARY_PATH := bin/$(BINARY_NAME)

.PHONY: build
build:
	@mkdir -p bin
	go build -o $(BINARY_PATH) ./cmd/xds-backend-extension-server

.PHONY: docker-build
docker-build:
	docker build -t $(IMAGE_REPO):$(IMAGE_TAG) .

.PHONY: generate
generate: generate-go generate-proto generate-controller-gen

.PHONY: generate-go
generate-go:
	go generate ./...

.PHONY: generate-proto
generate-proto:
	go tool buf generate

.PHONY: generate-controller-gen
generate-controller-gen:
	mkdir -p charts/xds-backend/crds
	go tool controller-gen object crd paths="./api/..." output:crd:dir=./charts/xds-backend/crds output:object:dir=./api/v1alpha1

.PHONY: buf-dep-update
buf-dep-update:
	go tool buf dep update

.PHONY: test
test:
	@mkdir -p bin
	go test -v -coverprofile=bin/coverage.out -covermode=atomic ./cmd/... ./internal/... ./pkg/...

.PHONY: test-coverage
test-coverage: test
	@mkdir -p bin
	go tool cover -html=bin/coverage.out -o bin/coverage.html
	@echo "Coverage report generated: bin/coverage.html"
	@echo "Coverage summary:"
	@go tool cover -func=bin/coverage.out | tail -1

ENVOY_GATEWAY_IMAGE ?=
RUN_EXPERIMENTAL ?= false

.PHONY: test-e2e
test-e2e: docker-build
	@EXPERIMENTAL_FLAG=""; \
	if [ "$(RUN_EXPERIMENTAL)" = "true" ]; then \
		EXPERIMENTAL_FLAG="-experimental"; \
	fi; \
	if [ -n "$(ENVOY_GATEWAY_IMAGE)" ]; then \
		echo "Running e2e tests with Envoy Gateway image: $(ENVOY_GATEWAY_IMAGE)"; \
		go test -v ./test/e2e/... -ginkgo.v -timeout 30m -envoy-gateway-image="$(ENVOY_GATEWAY_IMAGE)" $$EXPERIMENTAL_FLAG; \
	else \
		echo "Running e2e tests with default Envoy Gateway image from Helm chart"; \
		go test -v ./test/e2e/... -ginkgo.v -timeout 30m $$EXPERIMENTAL_FLAG; \
	fi

.PHONY: clean
clean:
	rm -rf test/**/.rendered-configs/
	rm -rf test/**/.kubeconfig/
	rm -rf test/**/.logs/
	rm -rf test/**/.helm/
	rm -rf test/**/.cache/
	rm -rf bin/
