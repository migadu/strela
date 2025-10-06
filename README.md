# Fune - High-Performance SMTP Delivery Service

A production-ready, queue-based SMTP delivery service with direct MX delivery, intelligent retry logic, IP reputation management, and comprehensive monitoring.

## Features

### Core Capabilities
- **Queue-Based Architecture**: Accepts messages via HTTP, returns immediately (200 OK for new messages, 202 Accepted for idempotent duplicates), processes asynchronously
- **Direct MX Delivery**: Bypasses SMTP relay, delivers directly to recipient's MX servers with IPv6-first support
- **DKIM Signing**: Optional DKIM signature support with 1024/2048-bit RSA keys
- **Multiple Source IPs**: Rotate through multiple outbound IPs (round-robin, random, hash-domain)
- **IP Reputation Tracking**: Automatically detects and manages degraded IPs due to blacklisting
- **Exponential Backoff**: Intelligent retry with 5min → 12h cap over 48 hours
- **Circuit Breaker**: Automatic health monitoring and fast-fail during infrastructure failures
- **Webhook Callbacks**: Notifies your endpoint on delivery/bounce events with retry logic

### Operations & Monitoring
- **Hot Reload**: Configuration changes without downtime (SIGHUP signal)
- **Prometheus Metrics**: Comprehensive metrics for monitoring delivery, queue, circuit breaker, and IP reputation
- **Panic Recovery**: Comprehensive crash prevention with safe goroutine execution
- **Graceful Shutdown**: Completes in-flight deliveries before shutting down
- **Admin CLI**: Queue inspection, statistics, and server management
- **Structured Logging**: JSON or console output with configurable levels

### Reliability & Performance
- **SQLite WAL**: Persistent queue with concurrent reads, single writer
- **Event-Driven Workers**: Instant message processing with fallback polling
- **MX Caching**: Reduces DNS queries and improves delivery latency
- **Destination Throttling**: Prevents rapid-fire connections (anti-spam)
- **TLS Support**: Optional HTTPS for API, opportunistic STARTTLS for SMTP
- **Idempotency**: Optional idempotency key support to prevent duplicate deliveries

## Quick Start

```bash
# Build binaries
make all

# Copy and edit configuration
cp config.toml.example config.toml
nano config.toml

# Run server
./fune-server

# Check queue status
./fune-admin queue

# Send test message
curl -X POST http://localhost:8080/v1/messages \
  -H "Authorization: Bearer your-token" \
  -H "Content-Type: application/json" \
  -d '{
    "from": "sender@example.com",
    "to": "recipient@example.com",
    "subject": "Test",
    "text": "Hello World"
  }'
```

## Configuration

### Minimal Configuration

```toml
[server]
database_path = "./queue.db"

[http]
listen = ":8080"
auth_token = "your-secret-token"

[queue]
worker_count = 10

[delivery]
source_ips = ["192.168.1.100"]
max_message_age_hours = 48

[callbacks]
webhook_url = "https://your-app.com/webhooks/delivery"
auth_token = "webhook-secret"
```

### Complete Configuration

See [config.toml.example](config.toml.example) for all options including:
- TLS/HTTPS configuration
- IP rotation strategies (round-robin, random, hash-domain)
- Circuit breaker thresholds
- DNS resolver settings
- Retry schedules and backoff
- Idempotency settings
- IP reputation tracking
- Prometheus metrics

### Hot Reload

Reload configuration without downtime:

```bash
# Send SIGHUP signal
kill -HUP $(cat fune.pid)

# Or with systemd
systemctl reload fune

# Or with admin CLI
./fune-admin reload -pid fune.pid
```

**Reloadable**: Source IPs, rate limits, circuit breaker, DNS, TLS certs, HTTP timeouts
**Non-reloadable**: Database path, listen address, worker count (require restart)

## API Usage

### Submit Message (JSON)

```bash
curl -X POST http://localhost:8080/v1/messages \
  -H "Authorization: Bearer your-token" \
  -H "Content-Type: application/json" \
  -d '{
    "from": "sender@example.com",
    "to": "recipient@example.com",
    "subject": "Test Subject",
    "text": "Plain text body",
    "html": "<p>HTML body</p>"
  }'
```

**Response (200 OK):**
```json
{
  "message_id": "msg_679d8a4c2f4h3k9d2j",
  "status": "queued",
  "queued_at": "2025-01-15T10:30:00Z"
}
```

### Submit with DKIM Signature

```bash
# Generate DKIM key pair
openssl genrsa -out dkim_private.pem 2048
openssl rsa -in dkim_private.pem -pubout -out dkim_public.pem

# Add DNS TXT record
# default._domainkey.example.com TXT "v=DKIM1; k=rsa; p=MIIBIjANB..."

# Send with DKIM
curl -X POST http://localhost:8080/v1/messages \
  -H "Authorization: Bearer your-token" \
  -H "Content-Type: application/json" \
  -d '{
    "from": "sender@example.com",
    "to": "recipient@example.com",
    "subject": "DKIM Signed Email",
    "text": "This email is DKIM signed",
    "dkim_private_key": "'"$(cat dkim_private.pem)"'",
    "dkim_selector": "default"
  }'
```

### Idempotent Requests

Prevent duplicate deliveries using idempotency keys:

```bash
curl -X POST http://localhost:8080/v1/messages \
  -H "Authorization: Bearer your-token" \
  -H "X-Idempotency-Key: unique-key-123" \
  -H "Content-Type: application/json" \
  -d '{"from": "sender@example.com", "to": "recipient@example.com", ...}'

# Sending again with same key returns existing message_id
# First request: 200 OK (queued)
# Duplicate request: 202 Accepted (idempotent)
```

Enable in config:
```toml
[http]
idempotency_enabled = true
idempotency_ttl_hours = 24
```

## Webhook Callbacks

Fune sends POST requests to your `webhook_url` for all terminal events:

### Delivered (Success)

```json
{
  "event_type": "delivered",
  "message_id": "msg_abc123",
  "from_address": "sender@example.com",
  "to_address": "recipient@example.com",
  "subject": "Test",
  "timestamp": "2025-01-15T10:35:00Z",
  "smtp_code": 250,
  "smtp_response": "OK",
  "mx_host": "mx1.example.com",
  "source_ip": "192.168.1.100",
  "attempts": 1
}
```

### Bounced (Permanent Failure)

```json
{
  "event_type": "bounced",
  "message_id": "msg_xyz789",
  "from_address": "sender@example.com",
  "to_address": "invalid@example.com",
  "timestamp": "2025-01-15T10:35:00Z",
  "smtp_code": 550,
  "smtp_response": "User not found",
  "error_message": "Permanent delivery failure",
  "attempts": 1
}
```

### Failed (Temporary, Retrying)

```json
{
  "event_type": "failed",
  "message_id": "msg_def456",
  "timestamp": "2025-01-15T10:35:00Z",
  "smtp_code": 421,
  "smtp_response": "Greylisted",
  "attempts": 2,
  "next_retry_at": "2025-01-15T10:37:00Z"
}
```

### Expired (Timeout)

```json
{
  "event_type": "expired",
  "message_id": "msg_ghi789",
  "timestamp": "2025-01-17T10:30:00Z",
  "error_message": "Message exceeded 48 hour delivery window",
  "attempts": 12
}
```

## Retry Schedule

Exponential backoff with configurable cap:

| Attempt | Delay | Cumulative |
|---------|-------|------------|
| 1 | Immediate | 0 |
| 2 | 5 min | 5 min |
| 3 | 10 min | 15 min |
| 4 | 20 min | 35 min |
| 5 | 40 min | 1h 15m |
| 6 | 80 min | 2h 35m |
| 7 | 160 min | 5h 15m |
| 8+ | 12h (capped) | → 48h max |

**Special cases:**
- **Greylisting (421)**: Aggressive 2-minute retry
- **Permanent (5xx)**: No retry, immediate bounce callback
- **Network errors**: Retry with backoff
- **Throttled**: Quick retry once rate limit window passes

## IP Reputation Management

Automatic detection and management of degraded IPs:

### How It Works

1. Delivery fails with reputation error (e.g., "550 blocked by Spamhaus")
2. IP automatically marked as "degraded"
3. IP removed from rotation pool
4. Alert sent to reputation webhook
5. After 48 hours (configurable), IP is retried
6. If successful → marked "recovered", alert sent
7. If failed → remains degraded for another 48 hours

### Configuration

```toml
[reputation]
enable_ip_tracking = true
alert_webhook_url = "https://your-app.com/api/reputation-alert"
alert_auth_token = "secret"
degraded_retry_hours = 48
```

### Reputation Alert Payload

```json
{
  "timestamp": "2025-01-15T10:30:00Z",
  "source_ip": "192.168.1.100",
  "event_type": "degraded",
  "from": "sender@example.com",
  "to": "recipient@example.com",
  "smtp_code": 550,
  "smtp_response": "IP blocked by Spamhaus RBL",
  "mx_host": "mx.example.com",
  "retry_after": "2025-01-17T10:30:00Z",
  "degraded_ips_count": 1
}
```

Event types: `degraded`, `recovered`

## Circuit Breaker

Automatically stops accepting requests when local infrastructure fails:

### States

- **Closed** (normal): All requests accepted
- **Open** (failing): Requests rejected with 503
- **Half-Open** (testing): Limited requests to test recovery

### Configuration

```toml
[delivery]
circuit_breaker_enabled = true
circuit_breaker_failure_threshold = 5      # Failures before opening
circuit_breaker_success_threshold = 2      # Successes to close
circuit_breaker_open_timeout_seconds = 60  # Wait before testing
```

### Triggers

**Opens on:**
- Network failures (connection refused, timeouts)
- DNS failures (NXDOMAIN, resolver unreachable)
- Local IP binding failures

**Does NOT open on:**
- Remote SMTP errors (4xx, 5xx codes)
- Greylisting or rate limiting

## Monitoring

### Prometheus Metrics

Available at `/metrics` endpoint:

```promql
# Queue depth by status
fune_queue_depth{status="pending"}

# Delivery attempts by outcome
fune_delivery_attempts_total{outcome="success"}
fune_delivery_attempts_total{outcome="permanent_error"}
fune_delivery_attempts_total{outcome="temporary_error"}

# Delivery duration (histogram)
fune_delivery_duration_seconds

# Circuit breaker state (0=closed, 1=half-open, 2=open)
fune_circuit_breaker_state

# IP reputation status
fune_ip_reputation_degraded{source_ip="192.168.1.100"}
fune_ip_reputation_events_total{event_type="degraded",source_ip="192.168.1.100"}

# HTTP requests
fune_http_requests_total{method="POST",path="/",status="200"}
fune_http_requests_total{method="POST",path="/",status="202"}  # idempotent responses

# Callback attempts
fune_callback_attempts_total{outcome="success",event_type="delivered"}
```

### Recommended Alerts

```yaml
# Circuit breaker open (infrastructure failure)
- alert: CircuitBreakerOpen
  expr: fune_circuit_breaker_state == 2
  for: 1m

# Source IP degraded due to reputation
- alert: SourceIPDegraded
  expr: fune_ip_reputation_degraded == 1
  for: 5m

# Queue backlog growing
- alert: QueueBacklogGrowing
  expr: fune_queue_depth{status="pending"} > 1000
  for: 10m

# Low delivery success rate
- alert: LowDeliverySuccessRate
  expr: rate(fune_delivery_attempts_total{outcome="success"}[5m]) / rate(fune_delivery_attempts_total[5m]) < 0.8
  for: 15m
```

### Admin CLI

```bash
# Queue statistics
./fune-admin queue

# Output:
# Total messages: 152
# Pending: 45
# Processing: 3
# Delivered: 98
# Hard Bounces: 4
# Expired: 2

# Queue details with pagination
./fune-admin queue -db queue.db -json | jq

# Top domains in queue
./fune-admin queue-domains

# Top senders
./fune-admin queue-senders

# Recent failures
./fune-admin failures

# Callback queue status
./fune-admin callbacks

# Throughput statistics
./fune-admin throughput

# Reload configuration
./fune-admin reload -pid fune.pid

# Check version
./fune-admin version
```

## Deployment

### systemd Service

Create `/etc/systemd/system/fune.service`:

```ini
[Unit]
Description=Fune SMTP Delivery Service
After=network.target

[Service]
Type=simple
User=fune
WorkingDirectory=/opt/fune
ExecStart=/opt/fune/fune-server
ExecReload=/bin/kill -HUP $MAINPID
Restart=always
RestartSec=5
PIDFile=/opt/fune/fune.pid

# Security hardening
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=true
ReadWritePaths=/opt/fune

[Install]
WantedBy=multi-user.target
```

```bash
sudo systemctl enable fune
sudo systemctl start fune
sudo systemctl status fune

# Reload configuration
sudo systemctl reload fune

# View logs
sudo journalctl -u fune -f
```

### Horizontal Scaling

Run multiple instances behind a load balancer:

```
        ┌─────────────┐
        │ Load Balancer│ (nginx/HAProxy)
        └──────┬───────┘
               │
    ┌──────────┼──────────┬──────────┐
    │          │          │          │
┌───▼───┐  ┌───▼───┐  ┌───▼───┐  ┌───▼───┐
│Inst 1 │  │Inst 2 │  │Inst 3 │  │Inst 4 │
│:8081  │  │:8082  │  │:8083  │  │:8084  │
└───┬───┘  └───┬───┘  └───┬───┘  └───┬───┘
    │          │          │          │
queue1.db  queue2.db  queue3.db  queue4.db
```

Each instance:
- Has its own SQLite database (shared-nothing architecture)
- Processes its own queue independently
- No coordination needed between instances
- Scales linearly with traffic

**HAProxy config with idempotency:**
```
backend fune
    balance hdr(X-Idempotency-Key)
    hash-type consistent
    server fune1 127.0.0.1:8081 check
    server fune2 127.0.0.1:8082 check
    server fune3 127.0.0.1:8083 check
    server fune4 127.0.0.1:8084 check
```

## Requirements

### System Requirements
- Go 1.23+ (for building)
- Outbound SMTP access (port 25)
- Linux/macOS/BSD (Windows untested)

### Network Requirements
- Multiple IP addresses (optional, for IP rotation)
- Proper reverse DNS (PTR records) for all source IPs
- No port 25 blocking by ISP/cloud provider

### DNS Requirements
For best deliverability:
- **PTR records**: Reverse DNS for all source IPs
- **SPF records**: Authorize your IPs to send email
- **DKIM records**: If using DKIM signing (recommended)
- **DMARC policy**: Optional but recommended

Example DNS records:
```
# PTR (reverse DNS)
100.1.168.192.in-addr.arpa. PTR mail.example.com.

# SPF
example.com. TXT "v=spf1 ip4:192.168.1.100 -all"

# DKIM
default._domainkey.example.com. TXT "v=DKIM1; k=rsa; p=MIIBIjAN..."

# DMARC
_dmarc.example.com. TXT "v=DMARC1; p=none; rua=mailto:dmarc@example.com"
```

## Testing

```bash
# Run all tests
go test ./...

# Run with race detection
go test -race ./...

# Run with coverage
go test -cover ./...

# Run specific package tests
go test ./internal/delivery -v

# Run specific test
go test ./internal/delivery -run TestIPReputationTracker
```

**Test Coverage**: 135+ unit tests including:
- Queue operations (9 tests)
- Delivery engine (90 tests, including 24 IP reputation tests)
- Callback system (14 tests)
- Worker pool (13 tests)
- HTTP handler (4 tests)
- Configuration (5 tests)
- Integration tests

## Architecture

```
HTTP Request → Handler → Queue (SQLite) → Returns 200 OK (or 202 if idempotent)
                             ↓
                    Event Notification
                             ↓
                     Worker Pool (concurrent)
                             ↓
                Filter Degraded IPs (reputation tracker)
                             ↓
                   Select Source IP (rotator)
                             ↓
              MX Lookup (DNS with caching)
                             ↓
           Circuit Breaker Check (if enabled)
                             ↓
              Destination Throttle Check
                             ↓
            Direct SMTP Delivery (IPv6 first)
                             ↓
          ┌──────────────────┴──────────────────┐
          ▼                                     ▼
      Success                               Failure
          │                                     │
          ├─ Update metrics                     ├─ Classify error
          ├─ Mark as delivered                  ├─ Update metrics
          ├─ Queue callback                     ├─ Check if reputation error
          └─ Record reputation (if degraded IP) │   └─ Mark IP degraded
                                                ├─ Schedule retry or mark terminal
                                                └─ Queue callback

                             ↓
                    Callback Handler
                             ↓
              POST to webhook with retry
                             ↓
                      Success → Delete
```

## Error Classification

### Permanent Errors (5xx) - No Retry
- **550**: User not found, mailbox unavailable
- **551**: User not local, will not relay
- **552**: Message too large, mailbox full
- **553**: Invalid mailbox name
- **554**: Transaction failed

### Temporary Errors (4xx) - Retry with Backoff
- **421**: Service unavailable (greylisting - aggressive retry)
- **450**: Mailbox busy
- **451**: Local error, rate limiting
- **452**: Insufficient storage
- **454**: TLS not available

### Reputation Errors - IP Degradation
Triggered by SMTP responses containing keywords:
- "blocked", "blacklist", "rbl", "dnsbl"
- "poor reputation", "spamhaus", "barracuda"
- "rejected for policy reasons"

### Network Errors - Retry with Backoff
- DNS failures (NXDOMAIN, SERVFAIL)
- Connection refused, timeout
- TLS handshake failures

## Troubleshooting

### Deliveries Not Working

1. **Check circuit breaker status:**
   ```bash
   curl http://localhost:8080/health
   # Should return {"status": "healthy", "circuit_breaker": "closed"}
   ```

2. **Check queue:**
   ```bash
   ./fune-admin queue
   # Look for stuck messages in "processing" or high retry counts
   ```

3. **Check logs:**
   ```bash
   journalctl -u fune -f
   # Look for network errors, DNS failures, or SMTP rejections
   ```

4. **Verify source IP can send mail:**
   ```bash
   telnet mx.example.com 25
   # Should connect successfully
   ```

### High Bounce Rate

1. **Check IP reputation:**
   ```bash
   # Check if IPs are on blacklists
   curl http://multirbl.valli.org/lookup/192.168.1.100.html
   ```

2. **Verify DNS records:**
   ```bash
   # Check SPF
   dig txt example.com

   # Check reverse DNS
   dig -x 192.168.1.100

   # Check DKIM (if using)
   dig txt default._domainkey.example.com
   ```

3. **Monitor metrics:**
   ```promql
   # Check delivery success rate
   rate(fune_delivery_attempts_total{outcome="success"}[5m])
   ```

### All IPs Degraded

If all source IPs are degraded due to reputation issues:

1. **Check reputation alerts:**
   - Review reputation webhook payloads
   - Check which blacklists are blocking your IPs

2. **Temporary fix:**
   - System automatically falls back to default IP (no binding)
   - Or disable reputation tracking temporarily:
     ```toml
     [reputation]
     enable_ip_tracking = false
     ```

3. **Long-term fix:**
   - Request delisting from blacklists
   - Improve email sending practices
   - Warm up new IPs gradually

## Performance Tuning

### Worker Count
```toml
[queue]
worker_count = 10  # Adjust based on CPU cores and delivery volume
```
- **Low traffic**: 5-10 workers
- **Medium traffic**: 10-20 workers
- **High traffic**: 20-50 workers per instance

### Batch Size
```toml
[queue]
batch_size = 5  # Messages per worker iteration
```
- Larger = fewer DB queries, but potential head-of-line blocking
- Smaller = more responsive, but more DB overhead

### MX Cache TTL
```toml
[delivery]
mx_cache_ttl_seconds = 3600  # 1 hour
```
- Longer = fewer DNS queries, faster delivery
- Shorter = more current MX records

## Documentation

- **README.md** (this file): User guide and quick reference
- **DOCUMENTATION.md**: Detailed technical documentation, architecture, design decisions
- **config.toml.example**: Complete configuration reference with comments

