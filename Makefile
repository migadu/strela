.PHONY: all clean build fune fune-admin install test 

# Binary names - can be overridden by environment variables
fune_BINARY ?= fune-server
fune_ADMIN_BINARY ?= fune-admin
fune_LINUX_BINARY ?= fune-linux-amd64
fune_ADMIN_LINUX_BINARY ?= fune-admin-linux-amd64
fune_FREEBSD_BINARY ?= fune-freebsd-amd64
fune_ADMIN_FREEBSD_BINARY ?= fune-admin-freebsd-amd64

# ====================================================================================
# Version Information
# You can override these variables during the build, e.g., make build VERSION=v1.0.0
# ====================================================================================
VERSION ?= $(shell git describe --tags --always --dirty --match='v*')
COMMIT ?= $(shell git rev-parse --short HEAD)
DATE ?= $(shell date -u +"%Y-%m-%dT%H:%M:%SZ")

# Go linker flags to inject version info
LDFLAGS_VARS = -X 'main.version=${VERSION}' -X 'main.commit=${COMMIT}' -X 'main.date=${DATE}'
LDFLAGS = -ldflags="${LDFLAGS_VARS}"

# Default target
all: build

# Build both executables
build: fune-server fune-admin

# Build the main fune server
fune-server:
	go build $(LDFLAGS) -o $(fune_BINARY) ./cmd/fune-server

# Build the fune-admin tool
fune-admin:
	go build $(LDFLAGS) -o $(fune_ADMIN_BINARY) ./cmd/fune-admin

# Install both executables to GOPATH/bin
install:
	go install ./cmd/fune-server
	go install ./cmd/fune-admin

# Clean build artifacts
clean:
	rm -f $(fune_BINARY) $(fune_ADMIN_BINARY) $(fune_LINUX_BINARY) $(fune_ADMIN_LINUX_BINARY) $(fune_FREEBSD_BINARY) $(fune_ADMIN_FREEBSD_BINARY)

# Run tests
test:
	go test -v ./...

# Run tests with coverage
coverage:
	go test -coverprofile=coverage.out ./...
	go tool cover -html=coverage.out

# Cross-compile with musl libc for Linux
linux-musl:
	CC=x86_64-linux-musl-gcc CXX=x86_64-linux-musl-g++ GOARCH=amd64 GOOS=linux go build -ldflags="${LDFLAGS_VARS} -extldflags -static" -o $(fune_LINUX_BINARY) ./cmd/fune-server
	CC=x86_64-linux-musl-gcc CXX=x86_64-linux-musl-g++ GOARCH=amd64 GOOS=linux go build -ldflags="${LDFLAGS_VARS} -extldflags -static" -o $(fune_ADMIN_LINUX_BINARY) ./cmd/fune-admin

# Cross-compile for FreeBSD
freebsd:
	GOARCH=amd64 GOOS=freebsd go build $(LDFLAGS) -o $(fune_FREEBSD_BINARY) ./cmd/fune-server
	GOARCH=amd64 GOOS=freebsd go build $(LDFLAGS) -o $(fune_ADMIN_FREEBSD_BINARY) ./cmd/fune-admin
