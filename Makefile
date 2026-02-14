.PHONY: all clean build fune-server fune-admin install test coverage build-linux freebsd help

# ====================================================================================
# Variables
# ====================================================================================

# Binary names
FUNE_SERVER_BIN ?= fune-server
FUNE_ADMIN_BIN ?= fune-admin

# Target directories
BIN_DIR ?= ./bin

# Go source paths
FUNE_SERVER_SRC = ./cmd/fune-server
FUNE_ADMIN_SRC = ./cmd/fune-admin

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
FUNE_SERVER_LINUX_BINARY ?= fune-linux-amd64
FUNE_SERVER_FREEBSD_BINARY ?= fune-freebsd-amd64
FUNE_ADMIN_LINUX_BINARY ?= fune-admin-linux-amd64
FUNE_ADMIN_FREEBSD_BINARY ?= fune-admin-freebsd-amd64


# ====================================================================================
# Default Target
# ====================================================================================

all: build

# ====================================================================================
# Build Targets
# ====================================================================================

build: fune-server fune-admin

fune-server:
	go build $(LDFLAGS) -o $(BIN_DIR)/$(FUNE_SERVER_BIN) $(FUNE_SERVER_SRC)

fune-admin:
	go build $(LDFLAGS) -o $(BIN_DIR)/$(FUNE_ADMIN_BIN) $(FUNE_ADMIN_SRC)

# ====================================================================================
# Installation and Cleanup
# ====================================================================================

install:
	go install $(FUNE_SERVER_SRC)

clean:
	rm -f $(BIN_DIR)/$(FUNE_SERVER_BIN)
	rm -f $(BIN_DIR)/$(FUNE_ADMIN_BIN)
	rm -f $(BIN_DIR)/$(FUNE_SERVER_LINUX_BINARY)
	rm -f $(BIN_DIR)/$(FUNE_SERVER_FREEBSD_BINARY)
	rm -f $(BIN_DIR)/$(FUNE_ADMIN_LINUX_BINARY)
	rm -f $(BIN_DIR)/$(FUNE_ADMIN_FREEBSD_BINARY)
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
	CGO_ENABLED=0 GOARCH=amd64 GOOS=linux go build -ldflags="${LDFLAGS_VARS}" -o $(BIN_DIR)/$(FUNE_SERVER_LINUX_BINARY) $(FUNE_SERVER_SRC)
	CGO_ENABLED=0 GOARCH=amd64 GOOS=linux go build -ldflags="${LDFLAGS_VARS}" -o $(BIN_DIR)/$(FUNE_ADMIN_LINUX_BINARY) $(FUNE_ADMIN_SRC)

freebsd:
	@mkdir -p $(BIN_DIR)
	GOARCH=amd64 GOOS=freebsd go build $(LDFLAGS) -o $(BIN_DIR)/$(FUNE_SERVER_FREEBSD_BINARY) $(FUNE_SERVER_SRC)
	GOARCH=amd64 GOOS=freebsd go build $(LDFLAGS) -o $(BIN_DIR)/$(FUNE_ADMIN_FREEBSD_BINARY) $(FUNE_ADMIN_SRC)

# ====================================================================================
# Help
# ====================================================================================

help:
	@echo "Usage: make [target]"
	@echo ""
	@echo "Targets:"
	@echo "  all                Build fune-server and fune-admin (default)"
	@echo "  build              Build fune-server and fune-admin"
	@echo "  fune-server        Build the fune-server binary"
	@echo "  fune-admin         Build the fune-admin binary"
	@echo "  install            Install binary to GOPATH/bin"
	@echo "  clean              Remove build artifacts"
	@echo "  test               Run tests"
	@echo "  coverage           Run tests with coverage"
	@echo "  build-linux        Cross-compile both binaries for Linux"
	@echo "  freebsd            Cross-compile both binaries for FreeBSD"
	@echo "  help               Show this help message"
