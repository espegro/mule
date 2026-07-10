BINARY ?= mule
VERSION ?= dev
GO ?= go
GOCACHE ?= /tmp/mule-go-cache
GOMODCACHE ?= /tmp/mule-go-mod
MIN_SAFE_GO ?= 1.26.5

.PHONY: check-go check-go-release build test test-race lint release

check-go:
	@version=`$(GO) env GOVERSION | sed 's/^go//'`; \
	first=`printf '%s\n%s\n' "$$version" "$(MIN_SAFE_GO)" | sort -V | head -n1`; \
	if [ "$$first" != "$(MIN_SAFE_GO)" ]; then \
		echo "warning: Go $$version has known standard-library vulnerabilities; use Go $(MIN_SAFE_GO) or newer" >&2; \
	fi

check-go-release:
	@version=`$(GO) env GOVERSION | sed 's/^go//'`; \
	first=`printf '%s\n%s\n' "$$version" "$(MIN_SAFE_GO)" | sort -V | head -n1`; \
	if [ "$$first" != "$(MIN_SAFE_GO)" ]; then \
		echo "error: release builds require Go $(MIN_SAFE_GO) or newer; found $$version" >&2; \
		exit 1; \
	fi

build: check-go
	mkdir -p bin
	GOCACHE=$(GOCACHE) GOMODCACHE=$(GOMODCACHE) $(GO) build -buildvcs=false -ldflags "-X main.version=$(VERSION)" -o bin/$(BINARY) ./cmd/mule

test: check-go
	GOCACHE=$(GOCACHE) GOMODCACHE=$(GOMODCACHE) $(GO) test ./...

test-race: check-go
	GOCACHE=$(GOCACHE) GOMODCACHE=$(GOMODCACHE) $(GO) test -race ./...

lint: check-go
	GOCACHE=$(GOCACHE) GOMODCACHE=$(GOMODCACHE) $(GO) vet ./...
	@if command -v staticcheck >/dev/null 2>&1; then staticcheck ./...; else echo "staticcheck not installed; skipping"; fi

release: check-go-release
	mkdir -p dist
	GOCACHE=$(GOCACHE) GOMODCACHE=$(GOMODCACHE) CGO_ENABLED=0 $(GO) build -buildvcs=false -trimpath -ldflags "-s -w -X main.version=$(VERSION)" -o dist/$(BINARY) ./cmd/mule
