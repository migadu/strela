.PHONY: all clean build strela-server strela-admin install test coverage build-linux freebsd help

# ====================================================================================
# Variables
# ====================================================================================

# Binary names
STRELA_SERVER_BIN ?= strela-server
STRELA_ADMIN_BIN ?= strela-admin

# Target directories
BIN_DIR ?= ./bin

# Go source paths
STRELA_SERVER_SRC = ./cmd/strela-server
STRELA_ADMIN_SRC = ./cmd/strela-admin

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
STRELA_SERVER_LINUX_BINARY ?= strela-linux-amd64
STRELA_SERVER_FREEBSD_BINARY ?= strela-freebsd-amd64
STRELA_ADMIN_LINUX_BINARY ?= strela-admin-linux-amd64
STRELA_ADMIN_FREEBSD_BINARY ?= strela-admin-freebsd-amd64


# ====================================================================================
# Default Target
# ====================================================================================

all: build

# ====================================================================================
# Build Targets
# ====================================================================================

build: strela-server strela-admin

strela-server:
	go build $(LDFLAGS) -o $(BIN_DIR)/$(STRELA_SERVER_BIN) $(STRELA_SERVER_SRC)

strela-admin:
	go build $(LDFLAGS) -o $(BIN_DIR)/$(STRELA_ADMIN_BIN) $(STRELA_ADMIN_SRC)

# ====================================================================================
# Installation and Cleanup
# ====================================================================================

install:
	go install $(STRELA_SERVER_SRC)

clean:
	rm -f $(BIN_DIR)/$(STRELA_SERVER_BIN)
	rm -f $(BIN_DIR)/$(STRELA_ADMIN_BIN)
	rm -f $(BIN_DIR)/$(STRELA_SERVER_LINUX_BINARY)
	rm -f $(BIN_DIR)/$(STRELA_SERVER_FREEBSD_BINARY)
	rm -f $(BIN_DIR)/$(STRELA_ADMIN_LINUX_BINARY)
	rm -f $(BIN_DIR)/$(STRELA_ADMIN_FREEBSD_BINARY)
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
	CGO_ENABLED=0 GOARCH=amd64 GOOS=linux go build -ldflags="${LDFLAGS_VARS}" -o $(BIN_DIR)/$(STRELA_SERVER_LINUX_BINARY) $(STRELA_SERVER_SRC)
	CGO_ENABLED=0 GOARCH=amd64 GOOS=linux go build -ldflags="${LDFLAGS_VARS}" -o $(BIN_DIR)/$(STRELA_ADMIN_LINUX_BINARY) $(STRELA_ADMIN_SRC)

freebsd:
	@mkdir -p $(BIN_DIR)
	GOARCH=amd64 GOOS=freebsd go build $(LDFLAGS) -o $(BIN_DIR)/$(STRELA_SERVER_FREEBSD_BINARY) $(STRELA_SERVER_SRC)
	GOARCH=amd64 GOOS=freebsd go build $(LDFLAGS) -o $(BIN_DIR)/$(STRELA_ADMIN_FREEBSD_BINARY) $(STRELA_ADMIN_SRC)

# ====================================================================================
# Help
# ====================================================================================

help:
	@echo "Usage: make [target]"
	@echo ""
	@echo "Targets:"
	@echo "  all                Build strela-server and strela-admin (default)"
	@echo "  build              Build strela-server and strela-admin"
	@echo "  strela-server      Build the strela-server binary"
	@echo "  strela-admin       Build the strela-admin binary"
	@echo "  install            Install binary to GOPATH/bin"
	@echo "  clean              Remove build artifacts"
	@echo "  test               Run tests"
	@echo "  coverage           Run tests with coverage"
	@echo "  build-linux        Cross-compile both binaries for Linux"
	@echo "  freebsd            Cross-compile both binaries for FreeBSD"
	@echo "  help               Show this help message"
