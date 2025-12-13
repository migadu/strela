.PHONY: all clean build fune-server install test coverage linux-musl freebsd help

# ====================================================================================
# Variables
# ====================================================================================

# Binary names
FUNE_SERVER_BIN ?= fune-server

# Target directories
CMD_DIR ?= ./cmd

# Go source paths
FUNE_SERVER_SRC = $(CMD_DIR)/fune-server

# Version Information
# Override with: make build VERSION=v1.0.0
VERSION ?= $(shell git describe --tags --always --dirty --match='v*')
COMMIT ?= $(shell git rev-parse --short HEAD)
DATE ?= $(shell date -u +"%Y-%m-%dT%H:%M:%SZ")

# Go linker flags
LDFLAGS_VARS = -X 'main.version=${VERSION}' -X 'main.commit=${COMMIT}' -X 'main.date=${DATE}'
LDFLAGS = -ldflags="${LDFLAGS_VARS}"

# Cross-compilation binaries
FUNE_LINUX_BINARY ?= fune-linux-amd64
FUNE_FREEBSD_BINARY ?= fune-freebsd-amd64


# ====================================================================================
# Default Target
# ====================================================================================

all: build

# ====================================================================================
# Build Targets
# ====================================================================================

build: fune-server

fune-server:
	go build $(LDFLAGS) -o $(CMD_DIR)/$(FUNE_SERVER_BIN) $(FUNE_SERVER_SRC)

# ====================================================================================
# Installation and Cleanup
# ====================================================================================

install:
	go install $(FUNE_SERVER_SRC)

clean:
	rm -f $(CMD_DIR)/$(FUNE_SERVER_BIN)
	rm -f $(CMD_DIR)/$(FUNE_LINUX_BINARY)
	rm -f $(CMD_DIR)/$(FUNE_FREEBSD_BINARY)
	rm -f coverage.out

# ====================================================================================
# Testing
# ====================================================================================

test:
	go test -v ./...

coverage:
	go test -coverprofile=coverage.out ./...
	go tool cover -html=coverage.out

security-scan:
	go install golang.org/x/vuln/cmd/govulncheck@latest
	govulncheck ./...

# ====================================================================================
# Cross-compilation
# ====================================================================================

linux-musl:
	CC=x86_64-linux-musl-gcc CXX=x86_64-linux-musl-g++ GOARCH=amd64 GOOS=linux go build -ldflags="${LDFLAGS_VARS} -extldflags -static" -o $(CMD_DIR)/$(FUNE_LINUX_BINARY) $(FUNE_SERVER_SRC)

freebsd:
	GOARCH=amd64 GOOS=freebsd go build $(LDFLAGS) -o $(CMD_DIR)/$(FUNE_FREEBSD_BINARY) $(FUNE_SERVER_SRC)

# ====================================================================================
# Help
# ====================================================================================

help:
	@echo "Usage: make [target]"
	@echo ""
	@echo "Targets:"
	@echo "  all                Build fune-server (default)"
	@echo "  build              Build fune-server"
	@echo "  fune-server        Build the fune-server binary"
	@echo "  install            Install binary to GOPATH/bin"
	@echo "  clean              Remove build artifacts"
	@echo "  test               Run tests"
	@echo "  coverage           Run tests with coverage"
	@echo "  linux-musl         Cross-compile for Linux with musl"
	@echo "  freebsd            Cross-compile for FreeBSD"
	@echo "  help               Show this help message"
