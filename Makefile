VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)
GOOS ?= $(shell go env GOOS)
GOARCH ?= $(shell go env GOARCH)

.PHONY: all build run test vet clean

all: build

build:
	CGO_ENABLED=0 GOOS=$(GOOS) GOARCH=$(GOARCH) go build -trimpath -ldflags "$(LDFLAGS)" -o dist/vigilant_$(GOOS)_$(GOARCH) .
	@ln -sf vigilant_$(GOOS)_$(GOARCH) dist/vigilant 2>/dev/null || true

run: build
	./dist/vigilant -c config.yaml

test:
	go test ./...

vet:
	go vet ./...

clean:
	rm -rf dist
