IMAGE_REPO := wtzhang23/xds-backend-extension-server
IMAGE_TAG := latest

.PHONY: docker-build
docker-build:
	docker build -t $(IMAGE_REPO):$(IMAGE_TAG) .

.PHONY: generate
generate:
	go tool buf generate

.PHONY: buf-dep-update
buf-dep-update:
	go tool buf dep update
