BINARY ?= mule
VERSION ?= dev
GO ?= go
GOCACHE ?= /tmp/mule-go-cache
GOMODCACHE ?= /tmp/mule-go-mod

.PHONY: build test test-race lint release

build:
	mkdir -p bin
	GOCACHE=$(GOCACHE) GOMODCACHE=$(GOMODCACHE) $(GO) build -buildvcs=false -ldflags "-X main.version=$(VERSION)" -o bin/$(BINARY) ./cmd/mule

test:
	GOCACHE=$(GOCACHE) GOMODCACHE=$(GOMODCACHE) $(GO) test ./...

test-race:
	GOCACHE=$(GOCACHE) GOMODCACHE=$(GOMODCACHE) $(GO) test -race ./...

lint:
	GOCACHE=$(GOCACHE) GOMODCACHE=$(GOMODCACHE) $(GO) vet ./...
	@if command -v staticcheck >/dev/null 2>&1; then staticcheck ./...; else echo "staticcheck not installed; skipping"; fi

release:
	mkdir -p dist
	GOCACHE=$(GOCACHE) GOMODCACHE=$(GOMODCACHE) CGO_ENABLED=0 $(GO) build -buildvcs=false -trimpath -ldflags "-s -w -X main.version=$(VERSION)" -o dist/$(BINARY) ./cmd/mule
