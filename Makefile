BINARY := ping-lens
PKG    := ./cmd/ping-lens
VERSION ?= $(shell git describe --always --dirty --tags 2>/dev/null || echo dev)

GOFLAGS := -trimpath
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build vet test run clean cross

build:
	go build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o bin/$(BINARY) $(PKG)

vet:
	go vet ./...

test:
	go test ./...

run: build
	./bin/$(BINARY) --config ./config.example.yaml

clean:
	rm -rf bin/

# Cross-compile for common Pi / server targets.
cross:
	GOOS=linux GOARCH=arm64 go build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o bin/$(BINARY)-linux-arm64 $(PKG)
	GOOS=linux GOARCH=amd64 go build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o bin/$(BINARY)-linux-amd64 $(PKG)
