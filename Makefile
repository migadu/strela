.PHONY: all clean build fune-server install test coverage build-linux freebsd help

# ====================================================================================
# Variables
# ====================================================================================

# Binary names
FUNE_SERVER_BIN ?= fune-server

# Target directories
BIN_DIR ?= ./bin

# Go source paths
FUNE_SERVER_SRC = ./cmd/fune-server

# Version Information
# Override with: make build VERSION=v1.0.0
VERSION ?= $(shell git describe --tags --always --dirty --match='v*')
COMMIT ?= $(shell git rev-parse --short HEAD)
DATE ?= $(shell date -u +"%Y-%m-%dT%H:%M:%SZ")

# Go linker flags
LDFLAGS_VARS = -X 'main.version=${VERSION}' -X 'main.commit=${COMMIT}' -X 'main.date=${DATE}'
LDFLAGS = -ldflags="${LDFLAGS_VARS}"

# Cross-compilation binaries
BIN_DIR ?= bin
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
	go build $(LDFLAGS) -o $(BIN_DIR)/$(FUNE_SERVER_BIN) $(FUNE_SERVER_SRC)

# ====================================================================================
# Installation and Cleanup
# ====================================================================================

install:
	go install $(FUNE_SERVER_SRC)

clean:
	rm -f $(BIN_DIR)/$(FUNE_SERVER_BIN)
	rm -f $(BIN_DIR)/$(FUNE_LINUX_BINARY)
	rm -f $(BIN_DIR)/$(FUNE_FREEBSD_BINARY)
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

build-linux:
	@mkdir -p $(BIN_DIR)
	CGO_ENABLED=0 GOARCH=amd64 GOOS=linux go build -ldflags="${LDFLAGS_VARS}" -o $(BIN_DIR)/$(FUNE_LINUX_BINARY) $(FUNE_SERVER_SRC)

freebsd:
	@mkdir -p $(BIN_DIR)
	GOARCH=amd64 GOOS=freebsd go build $(LDFLAGS) -o $(BIN_DIR)/$(FUNE_FREEBSD_BINARY) $(FUNE_SERVER_SRC)

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
	@echo "  build-linux        Cross-compile static binary for Linux"
	@echo "  freebsd            Cross-compile for FreeBSD"
	@echo "  help               Show this help message"
