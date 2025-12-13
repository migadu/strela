# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

Fune is a production-ready, **synchronous** SMTP delivery gateway written in Go. It accepts email messages via HTTP API and delivers them **immediately** to recipient MX servers, returning the SMTP result as a JSON response. No queuing, no async processing, no callbacks.

**Version**: v2.0.0 (Synchronous Architecture)
**Previous Version**: v1.x used async queue-based architecture (now deprecated)

## Build & Test Commands

### Building
```bash
make build           # Build fune-server
make clean          # Remove build artifacts

# Or manually:
go build -o fune-server cmd/fune-server/main.go
```

### Testing
```bash
go test ./...                           # Run all tests
go test ./internal/delivery/...         # Run tests for specific package
go test -run TestSpecificTest ./...     # Run specific test by name
go test -v ./...                        # Verbose test output
go test -race ./...                     # Run with race detector (important!)
make test                               # Run all tests via Makefile
make coverage                           # Generate coverage report (opens in browser)
```

### Running
```bash
./fune-server                           # Run server (uses config.toml)
./fune-server -config custom.toml       # Run with custom config
./fune-server -version                  # Show version information
```

## Architecture Overview (v2.0 - Synchronous)

### Request Flow
```
HTTP POST /v1/deliver
  ↓
Panic Recovery Middleware (catches all panics, returns 500, server continues)
  ↓
Security Headers Middleware
  ↓
Metrics Middleware (optional - records request metrics)
  ↓
Rate Limiting Middleware (optional - per-IP rate limiting)
  ↓
Concurrency Limit Middleware (optional - rejects if at capacity)
  ↓
Auth Middleware (bearer token)
  ↓
Handler validates request
  ↓
Create context with delivery timeout (default: 30s)
  ↓
Delivery Engine performs SMTP delivery (synchronously)
  ↓ (DNS lookup → MX selection → IP selection → SMTP conversation)
  ↓
Return JSON response immediately:
{
  "status": "delivered|temp_fail|hard_bounce|timeout|error",
  "smtp_code": 250,
  "smtp_message": "2.0.0 OK",
  "mx_host": "mx1.example.com",
  "source_ip": "192.168.1.100",
  "attempt_duration_ms": 1234
}
```

### Core Design Principles

1. **Synchronous by Design**: HTTP API blocks until SMTP delivery completes (or times out), then returns result immediately.
   - No background workers
   - No queue persistence
   - No webhook callbacks
   - Caller gets immediate feedback

2. **Stateless Architecture**: No database, no shared state between instances.
   - Perfect for horizontal scaling
   - In-memory caching only (DNS, MX, IP reputation)
   - Ephemeral state lost on restart (no impact on correctness)

3. **Concurrency Model**:
   - Each HTTP request runs in its own goroutine (Go's http.Server)
   - Optional concurrency limit via middleware (`max_concurrent_requests`)
   - Per-domain rate limiting prevents hammering same MX server

4. **Context Propagation**: All operations accept `context.Context` for timeout and cancellation.
   - `delivery_timeout_seconds` enforced via context deadline
   - Timeout cascade: Client → Load Balancer → Fune → SMTP

5. **IPv6/IPv4 Dual-Stack with Preference**:
   - Separate IPv4 and IPv6 source IP pools
   - IPv6-first delivery (configurable via `prefer_ipv6`)
   - IPv4-only or IPv6-only deployment supported
   - CIDR subnet expansion (e.g., `192.0.2.0/28`, `2001:db8::/124`)
   - Automatic IP version matching with MX host addresses

6. **Panic Recovery & Server Stability** (CRITICAL):
   - ✅ **All HTTP handlers** wrapped with panic recovery middleware
   - ✅ **All goroutines** wrapped with `recovery.SafeGo()`
   - ✅ **Type assertions** checked before use (no unchecked type assertions)
   - ✅ **Nil pointer checks** on critical paths
   - **Impact**: Server never crashes from panics - logs stack trace and continues
   - **Locations**:
     - HTTP handlers: `internal/handler/panic_recovery.go`
     - Goroutines: HTTP server, rate limiter cleanup, IP reputation alerts, TLS handshakes
     - Type assertions: Domain rate limiters, MX cache entries

7. **Caller Responsibilities** (IMPORTANT):
   - ⚠️ **Caller MUST implement queue** (Fune is stateless)
   - ⚠️ **Caller MUST implement retry logic** (exponential backoff)
   - ⚠️ **Caller MUST track message IDs** (no idempotency in Fune)
   - ⚠️ **Caller MUST handle delivery results** (success/temp_fail/hard_bounce)

## Package Structure

### Commands (`cmd/`)
- **fune-server**: Main SMTP delivery gateway (only binary in v2.0)

**Removed in v2.0:**
- ~~fune-admin~~ - Admin CLI tool (no queue to manage)

### Core Packages (`internal/`)

#### `handler/`
- HTTP API for message submission (`POST /v1/deliver`)
- Request validation, authentication (bearer token)
- Middleware stack for security, metrics, rate limiting, panic recovery
- **Key files**:
  - `handler_new.go`: Main HTTP handler
  - `panic_recovery.go`: Panic recovery middleware (CRITICAL for stability)
  - `concurrency_middleware.go`: Concurrency limiting
  - `rate_limiter.go`: Per-IP rate limiting (now with panic-safe cleanup goroutine)
  - `security_headers.go`: Security headers middleware
  - `metrics_middleware.go`: Prometheus metrics middleware
  - `health.go`: Health check endpoint

**Removed in v2.0:**
- ~~Idempotency support~~ (caller's responsibility)
- ~~Circuit breaker integration~~ (not needed in sync model)

#### `delivery/`
- Synchronous SMTP delivery with timeout handling and IPv6/IPv4 dual-stack support
- **Key components**:
  - `delivery.go`: Main delivery orchestration (DeliverMessage returns DeliveryResult)
    - **STARTTLS Support**: Automatic TLS upgrade when supported by MX server
    - Manual STARTTLS implementation (sends command, reads 220, wraps in TLS)
    - TLS 1.2+ required, certificate verification enabled by default
    - Gracefully falls back to plaintext if STARTTLS not supported
    - **IPv6/IPv4 Logic**: Tries preferred IP version first, falls back to other if available
  - `dns_resolver.go`: Custom DNS resolver with round-robin, UDP→TCP fallback
  - `mx_lookup.go`: In-memory MX record caching with TTL
  - `ip_rotator.go`: IPv4/IPv6 source IP pools with selection strategies (round-robin, random, hash-domain)
  - `ip_reputation.go`: In-memory IP reputation tracking (per-IP version)
  - `error_classifier.go`: Classify SMTP errors (temp vs permanent)

**Removed in v2.0:**
- ~~retry_scheduler.go~~ - Retry logic (caller's responsibility)
- ~~circuit_breaker.go~~ - Circuit breaker (not applicable)

#### `config/`
- TOML-based configuration with hot reload support (SIGHUP)
- **Config sections**:
  - `[inbound]` - HTTP API settings
  - `[outbound]` - SMTP delivery settings (includes IPv4/IPv6 source IPs)
  - `[dns]` - DNS resolver config
  - `[tls]` - TLS/Let's Encrypt config
  - `[metrics]` - Prometheus config
  - `[health]` - Health endpoint config
  - `[reputation]` - IP reputation config
  - `[cluster]` - Cluster config (still supported for Let's Encrypt coordination)
  - `[arc]` - ARC signing config
  - `[srs]` - SRS config
- **Key files**:
  - `config.go`: Config structs
  - `ip_expansion.go`: CIDR subnet expansion logic
  - `reload.go`: Hot reload mechanism

**Removed in v2.0:**
- ~~`[server]`~~ - Database path (no database)
- ~~`[queue]`~~ - Worker count, batch size (no queue)
- ~~`[callbacks]`~~ - Webhook config (no callbacks)

#### `dkim/`
- DKIM email signing support
- **Key file**: `signer.go`

#### `arc/`
- ARC (Authenticated Received Chain) signing for email forwarding
- Prevents DMARC failures when forwarding authenticated emails

#### `srs/`
- SRS (Sender Rewriting Scheme) for envelope sender rewriting
- Prevents SPF failures when forwarding email

#### `tls/`
- TLS certificate management (file-based or Let's Encrypt)
- Let's Encrypt supports two storage backends:
  - File storage (`autocert.DirCache`) for single-node deployments
  - S3 storage for multi-node clusters with leader-based coordination
- Auto-renewal with configurable storage
- **Key files**:
  - `manager.go`: Certificate loading and monitoring
  - `storage/s3_cache.go`: S3-based certificate storage

#### `metrics/`
- Prometheus metrics exposition
- **Key metrics for v2.0**:
  - `fune_active_deliveries` - Current concurrent deliveries
  - `fune_deliveries_total` - Total deliveries by status
  - `fune_delivery_duration_seconds` - Histogram of delivery times
  - `fune_http_requests_rejected_capacity_total` - Concurrency rejections
  - DNS cache hit/miss rates
  - IP reputation tracking

**Removed metrics in v2.0:**
- ~~`queue_depth`~~ - No queue
- ~~`worker_active`~~ - No workers
- ~~`callbacks_sent_total`~~ - No callbacks
- ~~`circuit_breaker_state`~~ - No circuit breaker

#### `recovery/`
- Panic recovery utilities for production stability (CRITICAL)
- Prevents server crashes from panics in goroutines and HTTP handlers
- **Key functions**:
  - `RecoverPanic(logger, context)`: Defer-based panic recovery
  - `SafeGo(logger, context, fn)`: Launch goroutine with automatic panic recovery
  - `RecoverPanicWithCallback(logger, context, callback)`: Recovery with custom cleanup
- **Usage**: All goroutines MUST use `SafeGo()` or defer `RecoverPanic()`
- **Key file**: `recovery.go`

## Configuration Architecture

### Config Section Mapping
- `[inbound]` → `InboundConfig` (HTTP API settings)
  - **New in v2.0:** `max_concurrent_requests` - Concurrency limit
- `[outbound]` → `OutboundConfig` (SMTP delivery settings)
  - **New in v2.0:** `delivery_timeout_seconds` - Delivery timeout
  - **New in v2.0.4:** `source_ips_v4`, `source_ips_v6`, `prefer_ipv6` - IPv6/IPv4 dual-stack
  - **Removed:** `source_ips` (replaced by v4/v6 specific), circuit breaker, retry settings
- `[dns]` → `DNSConfig`
- `[tls]` → `TLSConfig` (includes `[tls.letsencrypt]`)
- `[metrics]` → `MetricsConfig`
- `[health]` → `HealthConfig`
- `[reputation]` → `ReputationConfig`
- `[cluster]` → `ClusterConfig` (optional, for Let's Encrypt S3 coordination)
- `[arc]` → `ARCConfig`
- `[srs]` → `SRSConfig`

### Hot Reloadable Settings
Trigger with: `kill -HUP <pid>` or `systemctl reload fune`

**Reloadable:**
- Source IPs (`source_ips_v4`, `source_ips_v6`)
- IP preference (`prefer_ipv6`)
- IP selection strategy (`source_ip_selection`)
- Rate limits (`per_domain_interval_seconds`)
- DNS settings (resolvers, cache TTL)
- TLS certificates (file-based auto-reload)
- HTTP timeouts
- Delivery timeout (`delivery_timeout_seconds`)
- Concurrency limit (`max_concurrent_requests`)

**Note**: CIDR subnet expansion happens on startup, not during hot reload

**Non-Reloadable** (require restart):
- HTTP listen address (`inbound.listen`)

**Removed in v2.0:**
- ~~database_path~~ (no database)
- ~~worker_count~~ (no workers)
- ~~webhook_url~~ (no callbacks)

### IPv6/IPv4 Source IP Configuration (v2.0.4)

```toml
[outbound]
# IPv6/IPv4 dual-stack configuration
prefer_ipv6 = true                          # Try IPv6 first, fallback to IPv4
source_ips_v4 = ["192.0.2.1", "192.0.2.0/28"]  # IPv4 IPs/subnets
source_ips_v6 = ["2001:db8::1", "2001:db8::/124"]  # IPv6 IPs/subnets

# CIDR subnet limits (enforced on startup):
# - IPv4: max /22 (1024 IPs)
# - IPv6: max /120 (256 IPs)
```

**IPv6/IPv4 Delivery Logic:**
- **Both configured + `prefer_ipv6=true`**: Try IPv6 → fallback to IPv4
- **Both configured + `prefer_ipv6=false`**: Try IPv4 → fallback to IPv6
- **Only IPv4 configured**: IPv4-only delivery (no IPv6 attempts)
- **Only IPv6 configured**: IPv6-only delivery (no IPv4 attempts)
- **None configured**: Uses system default IP (no source binding)

**CIDR Expansion:**
- Happens once on server startup
- Subnets expanded to individual IPs in memory
- Supports both individual IPs and CIDR ranges
- IP version auto-detected and classified

**Source IP Binding Failures:**
- Detected via errno check (EADDRNOTAVAIL, bind errors)
- Returns HTTP 500 with clear error message
- Indicates misconfiguration (IP not assigned to interface)

### Other v2.0 Configuration

```toml
[inbound]
max_concurrent_requests = 200    # Limit concurrent HTTP requests (0 = unlimited)

[outbound]
delivery_timeout_seconds = 30    # Maximum time for SMTP delivery
```

## Testing Conventions

### Test Organization
- Unit tests alongside source files (`*_test.go`)
- No integration tests currently (were queue-based in v1.x)
- Test fixtures use temporary directories (`t.TempDir()`)

### Common Test Patterns
```go
// Config setup (v2.0.4+)
cfg := &config.OutboundConfig{
    SourceIPsV4: []string{"192.168.1.100"},
    SourceIPsV6: []string{"2001:db8::1"},
    PreferIPv6: true,
    SourceIPSelection: "round-robin",
    DeliveryTimeoutSeconds: 30,
}

// Expanded IPs for NewDeliverer
expandedIPs := &config.ExpandedSourceIPs{
    IPv4: []string{"192.168.1.100"},
    IPv6: []string{"2001:db8::1"},
}

// Logger setup
logger := slog.Default()

// Create deliverer
deliverer := delivery.NewDeliverer(cfg, expandedIPs, mxLookup, logger, repCfg, arcCfg, srsCfg)
```

### Running Specific Tests
```bash
go test -run TestMXLookup ./internal/delivery/...
go test -run "TestDelivery.*" ./internal/delivery/...
go test -race ./...  # Always run race detector!
```

### Test Coverage Status

**Completed Tests (v2.0.2+):**
- ✅ Timeout tests (delivery, DNS, connection timeouts)
- ✅ Concurrency tests (100+ parallel requests)
- ✅ Concurrency limit enforcement
- ✅ Per-domain rate limiting under concurrency
- ✅ Context cancellation tests
- ✅ Response format validation
- ✅ Error handling tests
- ✅ IPv6/IPv4 delivery flow tests (v2.0.4)
- ✅ CIDR expansion tests (v2.0.4)

**Test Files:**
- `integration_test.go`: 13 comprehensive integration tests
- `internal/delivery/delivery_flow_test.go`: IPv6/IPv4 logic validation
- `internal/delivery/ipversion_test.go`: IP rotator tests
- `internal/config/ip_expansion_test.go`: CIDR expansion tests

## Important Conventions

### Panic Recovery (CRITICAL)
**Production Stability Rule**: All panics MUST be recovered to prevent server crashes.

**Panic recovery is implemented at multiple layers:**
1. **HTTP Handlers**:
   - `handler.PanicRecoveryMiddleware()` wraps all HTTP endpoints
   - Catches panics, logs stack trace, returns 500 to client
   - Applied to `/v1/deliver` and `/health` endpoints

2. **Goroutines**:
   - Use `recovery.SafeGo(logger, "context", fn)` for ALL new goroutines
   - Already protected: HTTP server, rate limiter cleanup, IP reputation alerts, TLS handshakes
   - Example:
     ```go
     recovery.SafeGo(logger, "background-task", func() {
         // ... task that might panic
     })
     ```

3. **Type Assertions**:
   - Always check type assertions from `sync.Map` and `interface{}`
   - Example:
     ```go
     limiter, ok := limiterI.(*domainRateLimiter)
     if !ok {
         logger.Error("type assertion failed", "type", fmt.Sprintf("%T", limiterI))
         return fmt.Errorf("internal error")
     }
     ```

4. **Nil Pointer Checks**:
   - Check pointers before dereferencing on critical paths
   - Especially important for optional parameters and interface returns

**When adding new code**:
- ❌ NEVER use bare `go func() { ... }()` - always use `recovery.SafeGo()`
- ❌ NEVER use unchecked type assertions like `x := val.(*Type)`
- ✅ ALWAYS check type assertions: `x, ok := val.(*Type)`
- ✅ ALWAYS add panic recovery to new HTTP handlers via middleware

### Error Handling
- Delivery engine returns `DeliveryResult` struct:
  ```go
  type DeliveryResult struct {
      Status            string `json:"status"`              // "delivered", "temp_fail", "hard_bounce", "timeout", "error"
      SMTPCode          int    `json:"smtp_code"`           // SMTP response code or 0
      SMTPMessage       string `json:"smtp_message"`        // SMTP response text
      MXHost            string `json:"mx_host"`             // MX server hostname
      SourceIP          string `json:"source_ip"`           // Source IP used
      AttemptDurationMs int64  `json:"attempt_duration_ms"` // Delivery duration
      Error             string `json:"error,omitempty"`     // Error details
  }
  ```
- Use `fmt.Errorf("message: %w", err)` for error wrapping
- Context cancellation should be checked before expensive operations
- Errors are logged but don't crash the server
- Panics are recovered and logged - server continues running

### Logging
- Use structured logging with `log/slog` (Go standard library)
- Log levels: DEBUG (DNS queries), INFO (delivery success), WARN (retries), ERROR (permanent failures)
- Include relevant context: `message_id`, `to_domain`, `attempt`, etc.

### Concurrency
- Per-domain rate limiting uses `sync.Map` with per-domain mutexes
- MX cache and IP reputation use `sync.RWMutex` for thread safety
- HTTP concurrency limiting uses buffered channel as semaphore

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
5. Add to hot reload list in README if applicable
6. Update `CLAUDE.md` if it changes architecture

### Adding a New Metric
1. Add counter/gauge/histogram to `internal/metrics/metrics.go`
2. Register in `NewMetrics()`
3. Call metric update at appropriate locations
4. Update Prometheus scrape config if needed
5. Document in `CLAUDE.md` and `PRODUCTION_READINESS_V2.md`

### Modifying Delivery Logic
1. Update `internal/delivery/delivery.go`
2. Add tests in `delivery_test.go`
3. Consider timeout implications
4. Update metrics if needed
5. Test with race detector: `go test -race ./internal/delivery/...`

## External Dependencies

Major dependencies (see `go.mod`):
- `github.com/emersion/go-message`: Email message parsing/construction
- `github.com/emersion/go-smtp`: SMTP client library
- `github.com/prometheus/client_golang`: Metrics
- `github.com/BurntSushi/toml`: Config parsing
- `github.com/aws/aws-sdk-go-v2`: S3 (for TLS storage if configured)
- `golang.org/x/crypto/acme/autocert`: Let's Encrypt
- `log/slog`: Structured logging (Go standard library)

**Removed in v2.0:**
- ~~`github.com/mattn/go-sqlite3`~~ - SQLite driver (no longer needed)
- ~~CGO dependency~~ - Can now build with `CGO_ENABLED=0`

## Documentation Files

- `README.md`: Quick start, API overview
- `DOCUMENTATION.md`: Detailed architecture, all features
- `REFACTORING_PROMPT.md`: Complete v2.0 refactoring specification
- `PRODUCTION_READINESS_V2.md`: Production readiness assessment for v2.0
- `CLAUDE.md`: This file (development guide)
- `config.toml.example`: Complete configuration reference

## Architecture Changes in v2.0 (IMPORTANT)

### What Was Removed
- ❌ SQLite queue system (`internal/queue/`)
- ❌ Worker pool (`internal/worker/`)
- ❌ Callback system (`internal/callback/`)
- ❌ Circuit breaker (`internal/delivery/circuit_breaker.go`)
- ❌ Retry scheduler (`internal/delivery/retry_scheduler.go`)
- ❌ Idempotency support (`internal/handler/idempotency.go`)
- ❌ `fune-admin` CLI tool (`cmd/fune-admin/`)

### What Was Added
- ✅ Synchronous delivery with immediate JSON response
- ✅ HTTP concurrency limiting middleware
- ✅ Configurable delivery timeout
- ✅ DeliveryResult struct with comprehensive status info
- ✅ Stateless architecture (no database)
- ✅ IPv6/IPv4 dual-stack with preference (v2.0.4)
- ✅ CIDR subnet expansion (v2.0.4)
- ✅ Source IP binding failure detection (v2.0.4)

### What Stayed (Core Components)
- ✅ SMTP delivery engine
- ✅ DNS resolution and MX lookup caching
- ✅ Source IP rotation
- ✅ IP reputation tracking (in-memory)
- ✅ Per-domain rate limiting
- ✅ TLS/Let's Encrypt support
- ✅ Metrics and health endpoints
- ✅ Hot reload (SIGHUP)
- ✅ DKIM/ARC/SRS signing

## Known Issues & TODOs

### Critical Issues
None - all critical technical issues have been resolved!

### Production Checklist
Before deploying v2.0:
- [x] Fix STARTTLS implementation - **COMPLETED** (v2.0.1)
- [x] Implement all missing tests - **COMPLETED** (v2.0.2)
  - 13 integration tests implemented in `integration_test.go`
  - All tests passing with race detector
  - Coverage: timeouts, concurrency, rate limiting, context cancellation, error handling
- [x] Comprehensive panic recovery - **COMPLETED** (v2.0.3)
  - HTTP handlers protected with panic recovery middleware
  - All goroutines wrapped with `recovery.SafeGo()`
  - Type assertions checked (no unchecked assertions)
  - Nil pointer checks on critical paths
  - Server never crashes from panics
- [x] IPv6/IPv4 dual-stack support - **COMPLETED** (v2.0.4)
  - Separate IPv4/IPv6 source IP pools
  - CIDR subnet expansion with safety limits
  - IPv6-first delivery with configurable preference
  - IPv4-only and IPv6-only deployment modes
  - Source IP binding failure detection
  - Comprehensive tests for all IP version scenarios
- [ ] Run load tests and document capacity
- [ ] Create operational runbook
- [ ] Document caller integration (retry logic examples)
- [ ] Update deployment artifacts (Dockerfile, systemd)

See `PRODUCTION_READINESS_V2.md` for complete production readiness assessment.

## Deployment Notes

### v2.0 Deployment Differences

**Simplified:**
- No database setup or backups needed
- No queue to monitor or manage
- Stateless → easy horizontal scaling
- Smaller binary (no SQLite dependency)
- Can use `CGO_ENABLED=0` for static builds

**New Requirements:**
- Caller MUST implement queue
- Caller MUST implement retry logic
- Load balancer MUST support long connections (90s+ timeout)
- Higher concurrent connections → increase file descriptor limits

**Example systemd service:**
```ini
[Service]
LimitNOFILE=65536  # High for concurrent connections
```

**Example load balancer (nginx):**
```nginx
proxy_read_timeout 90s;  # Must exceed delivery_timeout_seconds
proxy_connect_timeout 5s;
keepalive_timeout 90s;
```

## Troubleshooting Common Issues

### High Timeout Rate
- Check `fune_deliveries_total{status="timeout"}` metric
- Increase `delivery_timeout_seconds` if needed
- Check DNS resolver health
- Check target MX server response times

### Concurrency Rejections (503)
- Check `fune_http_requests_rejected_capacity_total` metric
- Increase `max_concurrent_requests`
- Add more Fune instances (horizontal scaling)

### High Latency
- Check `fune_delivery_duration_seconds` histogram
- P95 should be < 5s, P99 < 10s
- Check DNS cache hit rate
- Check per-domain rate limiting (may queue requests)

### Degraded IP Reputation
- Check `fune_ip_reputation_degraded` metric
- Review reputation alert webhook logs
- Investigate blacklisting (use online blacklist checkers)
- Rotate to different source IPs

## Important Reminders

1. **Always run tests with race detector**: `go test -race ./...`
2. **Panic recovery is MANDATORY**: Use `recovery.SafeGo()` for all goroutines, check all type assertions
3. **Caller is responsible for**: Queue, retry logic, message ID tracking, idempotency
4. **Load balancer timeout** must exceed `delivery_timeout_seconds`
5. **Stateless architecture**: State lost on restart (DNS cache, IP reputation, CIDR expansion)
6. **No backwards compatibility**: v2.0 is breaking change from v1.x
7. **STARTTLS is enabled**: Messages encrypted in transit when supported by MX server
8. **Server never crashes from panics**: All HTTP handlers and goroutines are protected
9. **IPv6/IPv4 configuration**: Use `source_ips_v4` and `source_ips_v6` (legacy `source_ips` removed in v2.0.4)
10. **CIDR expansion on startup**: Subnets expanded once at startup, hot reload doesn't re-expand

---

**Last Updated**: 2025-12-13 (v2.0.4 - IPv6/IPv4 dual-stack with CIDR expansion)
**Next Review**: After load testing and operational documentation
