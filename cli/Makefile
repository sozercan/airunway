.PHONY: build test cross

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
GIT_COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
BUILD_TIME ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
OUTPUT ?= ../dist/airunway$(if $(filter windows,$(GOOS)),.exe)
PKG = github.com/ai-runway/airunway/cli/internal/cli
LDFLAGS = -s -w -X $(PKG).Version=$(VERSION) -X $(PKG).GitCommit=$(GIT_COMMIT) -X $(PKG).BuildTime=$(BUILD_TIME)

build:
	CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o $(OUTPUT) ./cmd/airunway

test:
	go test ./...

cross:
	$(MAKE) build GOOS=linux GOARCH=amd64 OUTPUT=../dist/airunway-linux-x64
	$(MAKE) build GOOS=linux GOARCH=arm64 OUTPUT=../dist/airunway-linux-arm64
	$(MAKE) build GOOS=darwin GOARCH=amd64 OUTPUT=../dist/airunway-darwin-x64
	$(MAKE) build GOOS=darwin GOARCH=arm64 OUTPUT=../dist/airunway-darwin-arm64
	$(MAKE) build GOOS=windows GOARCH=amd64 OUTPUT=../dist/airunway-windows-x64.exe
