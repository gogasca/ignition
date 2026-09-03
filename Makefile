GO ?= go
BIN ?= bin
IMAGE_REGISTRY ?= us-central1-docker.pkg.dev/ignition-dev/ignition
IMAGE_TAG ?= dev

.PHONY: build test tidy fmt vet images push-images

build:
	$(GO) build -o $(BIN)/ignition-api ./cmd/ignition-api
	$(GO) build -o $(BIN)/ignition-controller ./cmd/ignition-controller
	$(GO) build -o $(BIN)/ignition-gateway ./cmd/ignition-gateway
	$(GO) build -o $(BIN)/ignitionctl ./cmd/ignitionctl
	$(GO) build -o $(BIN)/sandbox-init ./cmd/sandbox-init
	$(GO) build -o $(BIN)/ignition-gpu-agent ./cmd/ignition-gpu-agent
	CGO_ENABLED=1 $(GO) build -o $(BIN)/cuda-check ./cmd/cuda-check

test:
	$(GO) test ./...

# Optional override: use an existing disposable Postgres instead of Testcontainers.
#   docker run -d -p 5432:5432 -e POSTGRES_PASSWORD=ignition -e POSTGRES_DB=ignition postgres:16
#   IGNITION_TEST_DATABASE_URL=postgres://postgres:ignition@127.0.0.1:5432/ignition?sslmode=disable $(GO) test ./internal/store -count=1

tidy:
	$(GO) mod tidy

fmt:
	$(GO) fmt ./...

vet:
	$(GO) vet ./...

images:
	docker build -f deploy/docker/ignition-api.Dockerfile -t $(IMAGE_REGISTRY)/ignition-api:$(IMAGE_TAG) .
	docker build -f deploy/docker/ignition-controller.Dockerfile -t $(IMAGE_REGISTRY)/ignition-controller:$(IMAGE_TAG) .
	docker build -f deploy/docker/ignition-gateway.Dockerfile -t $(IMAGE_REGISTRY)/ignition-gateway:$(IMAGE_TAG) .
	docker build -f deploy/docker/ignition-prober.Dockerfile -t $(IMAGE_REGISTRY)/ignition-prober:$(IMAGE_TAG) .
	docker build -f deploy/docker/ignition-gpu-agent.Dockerfile -t $(IMAGE_REGISTRY)/ignition-gpu-agent:$(IMAGE_TAG) .

push-images: images
	docker push $(IMAGE_REGISTRY)/ignition-api:$(IMAGE_TAG)
	docker push $(IMAGE_REGISTRY)/ignition-controller:$(IMAGE_TAG)
	docker push $(IMAGE_REGISTRY)/ignition-gateway:$(IMAGE_TAG)
	docker push $(IMAGE_REGISTRY)/ignition-prober:$(IMAGE_TAG)
	docker push $(IMAGE_REGISTRY)/ignition-gpu-agent:$(IMAGE_TAG)

# Sandbox-init seed images: a minimal CPU image and a CUDA-based GPU image.
# CUDA_BASE must be a same-region Artifact Registry mirror of nvidia/cuda:*-base.
.PHONY: sandbox-images
sandbox-images:
	docker build -f images/sandbox-init/Dockerfile -t $(IMAGE_REGISTRY)/img_seed_cpu:$(IMAGE_TAG) .
	docker build -f images/sandbox-init/gpu.Dockerfile $(if $(CUDA_BASE),--build-arg CUDA_BASE=$(CUDA_BASE),) -t $(IMAGE_REGISTRY)/img_seed_gpu:$(IMAGE_TAG) .
