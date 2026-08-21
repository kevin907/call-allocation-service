BINARY := call-allocation-service
IMAGE  := $(BINARY):dev

.PHONY: help build test lint run docker verify-k8s

help:
	@echo "build   compile to bin/$(BINARY)"
	@echo "test    go test -race -cover ./..."
	@echo "lint    gofmt check and go vet"
	@echo "run     run the service on PORT (default 8080)"
	@echo "docker  build the container image as $(IMAGE)"
	@echo "verify-k8s  deploy to a throwaway kind cluster and assert it works"

build:
	go build -o bin/$(BINARY) ./cmd/callallocator

test:
	go test -race -cover ./...

lint:
	@test -z "$$(gofmt -l .)" || { echo "gofmt needed:"; gofmt -l .; exit 1; }
	go vet ./...

run:
	go run ./cmd/callallocator

docker:
	docker build -t $(IMAGE) .

verify-k8s:
	./scripts/verify-k8s.sh
