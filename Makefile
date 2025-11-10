IMAGE_REPO := wtzhang23/xds-backend-extension-server
IMAGE_TAG := latest

.PHONY: docker-build
docker-build:
	docker build -t $(IMAGE_REPO):$(IMAGE_TAG) .

.PHONY: generate
generate: generate-proto generate-controller-gen

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

.PHONY: clean
clean:
	rm -rf test/**/.rendered-configs/
	rm -rf test/**/.kubeconfig/
	rm -rf test/**/.logs/
	rm -rf test/**/.helm/
	rm -rf test/**/.cache/
