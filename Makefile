GO      ?= go
IMAGE   ?= marstek-prometheus-controller:dev
VERSION ?= dev

.PHONY: build test fmt lint run docker-build

build:
	$(GO) build -trimpath -ldflags="-X main.version=$(VERSION)" -o bin/marstek-controller ./cmd/marstek-controller

test:
	$(GO) test ./...

fmt:
	$(GO) fmt ./...

lint:
	$(GO) vet ./...

run:
	$(GO) run -ldflags="-X main.version=$(VERSION)" ./cmd/marstek-controller

docker-build:
	docker build --platform linux/amd64 --build-arg VERSION=$(VERSION) -t $(IMAGE) .
