# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

Fune is a production-ready, queue-based SMTP delivery service written in Go. It accepts email messages via HTTP API, queues them in SQLite, and delivers them directly to recipient MX servers with intelligent retry logic, IP reputation management, and webhook callbacks.

## Build & Test Commands

### Building
```bash
make build           # Build both fune-server and fune-admin
make fune-server     # Build only the server
make fune-admin      # Build only the admin tool
make clean          # Remove build artifacts
```

### Testing
```bash
go test ./...                           # Run all tests
go test ./internal/delivery/...         # Run tests for specific package
go test -run TestSpecificTest ./...     # Run specific test by name
go test -v ./...                        # Verbose test output
make test                               # Run all tests via Makefile
make coverage                           # Generate coverage report (opens in browser)
```

### Running
```bash
./fune-server                           # Run server (uses config.toml)
./fune-server -config custom.toml       # Run with custom config
./fune-admin queue                      # Show queue statistics
./fune-admin config                     # Show current configuration
./fune-admin health                     # Show health status
```

## Architecture Overview

### Request Flow
```
HTTP POST → Handler → Queue (SQLite) → Worker Pool → Delivery Engine → MX Server
                         ↓                               ↓
                    Callback Queue ← Callback Handler ← Delivery Result
```

### Core Design Principles

1. **Asynchronous by Design**: HTTP API returns `202 Accepted` immediately after queueing. Delivery happens asynchronously in background workers.

2. **Event-Driven**: Uses Go channels (`notifyCh`, `callbackNotifyCh`) for instant worker notifications when messages are queued, with 30s fallback polling.

3. **SQLite with WAL Mode**: Single-file database with Write-Ahead Logging enables concurrent reads during writes. No external dependencies.

4. **Context Propagation**: All operations accept `context.Context` for graceful cancellation and timeout handling.

5. **IPv6-First**: Delivery engine attempts IPv6 before falling back to IPv4.

## Package Structure

### Commands (`cmd/`)
- **fune-server**: Main SMTP delivery server
- **fune-admin**: CLI tool for queue management, stats, config display

### Core Packages (`internal/`)

#### `queue/`
- SQLite-backed persistent queue with WAL mode
- Message lifecycle: `queued` → `sending` → `delivered` | `hard_bounce` | `temp_expired` | `expired`
- Manages both message queue and callback queue
- **Key files**:
  - `queue.go`: Core queue operations (Enqueue, Dequeue, UpdateStatus)
  - `schema.go`: Database schema and migrations
  - Channel-based worker notification system

#### `handler/`
- HTTP API for message submission (`POST /v1/messages`)
- Request validation, authentication (bearer token)
- Idempotency support (distributed via gossip protocol)
- Circuit breaker integration (rejects requests when delivery circuit is open)
- **Key file**: `handler_new.go`

#### `delivery/`
- Direct MX delivery with IPv6/IPv4 support
- **Key components**:
  - `delivery.go`: Main delivery orchestration
  - `dns_resolver.go`: Custom DNS resolver with round-robin, UDP→TCP fallback
  - `mx_lookup.go`: MX record caching with configurable TTL
  - `ip_rotator.go`: Source IP selection (round-robin, random, hash-domain)
  - `ip_reputation.go`: Tracks degraded IPs, removes from rotation
  - `retry_scheduler.go`: Exponential backoff with greylist handling
  - `circuit_breaker.go`: Prevents accepting messages during delivery failures

#### `worker/`
- Worker pool that processes queued messages
- Each worker: dequeues batch → delivers → updates status → schedules retries
- Handles delivery errors, retry scheduling, and terminal states
- **Key file**: `worker.go`

#### `callback/`
- Webhook notifications for delivery events
- Separate queue with its own retry logic and circuit breaker
- **Key files**:
  - `callback.go`: Callback dispatcher
  - `circuit_breaker.go`: Protects against unreachable webhooks

#### `config/`
- TOML-based configuration with hot reload support (SIGHUP)
- **Important**: Config sections are `[inbound]` (HTTP API) and `[outbound]` (SMTP delivery)
- **Key files**:
  - `config.go`: Config structs (`InboundConfig`, `OutboundConfig`, etc.)
  - `reload.go`: Hot reload mechanism with validation

#### `gossip/`
- Optional clustering using HashiCorp memberlist
- Features: leader election (for Let's Encrypt), distributed idempotency, cluster health
- **Key file**: `gossip.go`

#### `dkim/`
- DKIM email signing support
- **Key file**: `signer.go`

#### `tls/`
- TLS certificate management (file-based or Let's Encrypt)
- Auto-renewal with S3 storage for multi-node clusters
- **Key files**:
  - `manager.go`: Certificate loading and monitoring
  - `storage/s3_cache.go`: S3-based certificate storage

#### `metrics/`
- Prometheus metrics exposition
- Tracks queue depth, delivery rates, circuit breaker state, etc.
- **Key file**: `metrics.go`

## Configuration Architecture

### Config Section Mapping
- `[inbound]` → `InboundConfig` (HTTP API settings)
- `[outbound]` → `OutboundConfig` (SMTP delivery settings)
- `[queue]` → `QueueConfig`
- `[dns]` → `DNSConfig`
- `[callbacks]` → `CallbacksConfig`
- `[tls]` → `TLSConfig`
- `[cluster]` → `ClusterConfig` (gossip protocol)
- `[metrics]` → `MetricsConfig`
- `[health]` → `HealthConfig`
- `[reputation]` → `ReputationConfig`

**Important**: When working with config, use `Inbound` and `Outbound` field names, not `HTTP` or `Delivery`.

### Hot Reloadable Settings
- Source IPs (`source_ips`)
- IP selection strategy (`source_ip_selection`)
- Rate limits (`per_domain_interval_seconds`)
- Circuit breaker thresholds
- DNS settings (resolvers, cache TTL)
- TLS certificates (file-based auto-reload)
- HTTP timeouts

### Non-Reloadable Settings (require restart)
- `database_path`
- HTTP listen address
- Worker count
- Webhook URL

## Testing Conventions

### Test Organization
- Unit tests alongside source files (`*_test.go`)
- Integration tests in root (`integration_test.go`)
- Test fixtures use temporary directories (`t.TempDir()`)

### Common Test Patterns
```go
// Queue setup
q, cleanup := queue.SetupTestQueue(t)
defer cleanup()

// Config setup
cfg := &config.OutboundConfig{
    SourceIPs: []string{"192.168.1.100"},
    SourceIPSelection: "round-robin",
    // ...
}

// Logger setup
logger, _ := zap.NewDevelopment()
```

### Running Specific Tests
```bash
go test -run TestMXLookup ./internal/delivery/...
go test -run "TestCircuitBreaker.*" ./internal/delivery/...
```

## Circuit Breaker Pattern

The system uses circuit breakers to prevent cascading failures:

1. **Delivery Circuit Breaker** (`internal/delivery/circuit_breaker.go`)
   - Opens after consecutive local failures (network errors, DNS failures)
   - Does NOT open on remote errors (SMTP 5xx from recipient server)
   - When open: HTTP API returns `503 Service Unavailable`

2. **Callback Circuit Breaker** (`internal/callback/circuit_breaker.go`)
   - Opens after consecutive webhook failures
   - When open: callbacks are postponed, not lost

### States
- **Closed**: Normal operation
- **Open**: Rejecting requests, waiting for timeout
- **Half-Open**: Testing recovery with limited requests

## IP Reputation System

Located in `internal/delivery/ip_reputation.go`:

1. SMTP 550/554 responses with reputation keywords trigger IP degradation
2. Degraded IPs are removed from rotation pool
3. Retry after configured hours (default: 48h)
4. Webhook alerts sent on degradation and recovery
5. Integration with IP rotator for automatic filtering

## DNS Caching Strategy

Two-level DNS caching in `internal/delivery/`:

1. **MX Record Cache** (`mx_lookup.go`)
   - Successful lookups: `dns.cache_ttl_seconds` (default: 1 hour)
   - Failed lookups: `dns.cache_negative_ttl_seconds` (default: 1 minute)

2. **DNS Resolver** (`dns_resolver.go`)
   - Round-robin across multiple DNS servers
   - UDP→TCP fallback on truncation
   - Supports IPv6 DNS servers

## Retry Logic

Implemented in `internal/delivery/retry_scheduler.go`:

- **Temporary Failures**: Exponential backoff (5min → 10min → 20min → ... → 12hr max)
- **Greylisting (421)**: Fast retry (2 minutes)
- **Permanent Failures (5xx)**: No retry, immediate hard bounce
- **Max Age**: Messages expire after 48 hours (configurable)

## Important Conventions

### Error Handling
- Use `fmt.Errorf("message: %w", err)` for error wrapping
- Context cancellation should be checked in long operations
- Database errors are logged but don't crash the server

### Logging
- Use structured logging with `zap`
- Log levels: DEBUG (DNS queries), INFO (delivery success), WARN (retries), ERROR (permanent failures)
- Include relevant context: `message_id`, `to_domain`, `attempt`, etc.

### Database Operations
- Always use `writeMu` for write operations to serialize SQLite writes
- Use prepared statements to prevent SQL injection
- WAL mode enables concurrent reads without blocking

### Config Validation
- Validation happens in `internal/config/reload.go`
- Non-reloadable changes are rejected with descriptive errors
- Use `SetDefaults()` for optional config values

## Common Development Tasks

### Adding a New Config Option
1. Add field to appropriate config struct in `internal/config/config.go`
2. Add TOML tag: `toml:"field_name"`
3. Set default in `SetDefaults()` if optional
4. Update `config.toml.example` with inline comment
5. Add to hot reload list in `README.md` if applicable

### Adding a New Metric
1. Add counter/gauge/histogram to `internal/metrics/metrics.go`
2. Register in `NewMetrics()`
3. Call metric update at appropriate locations
4. Update Prometheus scrape config if needed

### Modifying Retry Logic
1. Update `internal/delivery/retry_scheduler.go`
2. Add tests in `retry_scheduler_test.go`
3. Consider impact on `max_message_age_hours`

## SQLite Schema

See `internal/queue/schema.go` for complete schema. Key tables:

- **messages**: Main queue (id, message_id, status, next_retry_at, etc.)
- **delivery_attempts**: Audit trail of all delivery attempts
- **callbacks**: Webhook callback queue
- **mx_cache**: Cached MX records with TTL
- **idempotency_keys**: Prevents duplicate submissions
- **degraded_ips**: IP reputation tracking

## External Dependencies

Major dependencies (see `go.mod`):
- `github.com/mattn/go-sqlite3`: SQLite driver (CGO required)
- `github.com/emersion/go-message`: Email message parsing/construction
- `github.com/hashicorp/memberlist`: Gossip protocol for clustering
- `go.uber.org/zap`: Structured logging
- `github.com/prometheus/client_golang`: Metrics

## Documentation Files

- `README.md`: Quick start, API overview
- `DOCUMENTATION.md`: Detailed architecture, all features
- `config.toml.example`: Complete configuration reference
- `CLAUDE.md`: This file
