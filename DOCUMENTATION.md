# Fune SMTP Delivery Service - Technical Documentation

## Table of Contents

- [Architecture Overview](#architecture-overview)
- [Design Philosophy](#design-philosophy)
- [Component Details](#component-details)
  - [HTTP Handler](#http-handler)
  - [Queue System](#queue-system)
  - [Worker Pool](#worker-pool)
  - [Delivery Engine](#delivery-engine)
  - [DNS Resolver](#dns-resolver)
  - [Callback System](#callback-system)
  - [Configuration](#configuration)
- [Security Features](#security-features)
- [Error Handling & Retry Logic](#error-handling--retry-logic)
- [Circuit Breaker & Failover](#circuit-breaker--failover)
- [IP Reputation Tracking](#ip-reputation-tracking)
- [Anti-Spam Measures](#anti-spam-measures)
- [Observability & Metrics](#observability--metrics)
- [Performance Optimizations](#performance-optimizations)

---

## Architecture Overview

Fune is a high-performance, queue-based SMTP delivery service designed for reliable email delivery with proper retry logic, exponential backoff, and webhook callbacks. The architecture follows an event-driven design with clear separation of concerns.

### Component Flow

```
HTTP Request → Handler → Queue (SQLite) → Worker Pool → Delivery Engine → MX Servers
                              ↓                                ↓
                         Callback Queue ← Callback Handler ← Delivery Result
```

### Core Components

1. **HTTP Handler** - RESTful API for message submission
2. **Queue System** - SQLite-backed persistent queue with WAL mode
3. **Worker Pool** - Concurrent workers processing queued messages
4. **Delivery Engine** - Direct MX delivery with IPv6 support
5. **DNS Resolver** - Custom DNS resolution with caching
6. **Callback System** - Webhook notifications for delivery events

---

## Design Philosophy

### 1. **Asynchronous by Design**

The service immediately returns `202 Accepted` after validating and enqueuing messages. Actual delivery happens asynchronously in background workers. This prevents HTTP timeout issues and allows for proper retry handling.

**Why**: SMTP delivery can take seconds or fail requiring retries over hours/days. Synchronous delivery would tie up HTTP connections and prevent proper backoff.

### 2. **Event-Driven Architecture**

Instead of polling, the system uses Go channels for instant notifications when new messages or callbacks are queued. A fallback polling mechanism (30s default) ensures reliability.

**Why**: Reduces latency (messages start delivery immediately) and CPU usage (no constant polling).

### 3. **Persistent Queue with SQLite**

SQLite in WAL (Write-Ahead Logging) mode provides:
- Single writer + multiple concurrent readers
- ACID transactions without external dependencies
- Simple deployment (single file database)

**Why**: Redis/RabbitMQ add operational complexity. SQLite provides persistence, concurrency, and reliability in a zero-dependency package.

### 4. **Context Propagation**

All operations accept `context.Context` for graceful cancellation:
- HTTP server shutdown cancels in-flight deliveries
- DNS queries respect context timeouts
- SMTP connections can be interrupted

**Why**: Prevents hanging goroutines during shutdown and enables proper timeout handling throughout the stack.

### 5. **IPv6-First Delivery**

The delivery engine attempts IPv6 before IPv4, following modern internet standards.

**Why**: IPv6 is increasingly preferred by major mail providers and can offer better deliverability.

---

## Component Details

### HTTP Handler

**Location**: `internal/handler/handler_new.go`

#### Responsibilities

- Accept email messages via HTTP POST
- Validate message structure and size
- Authenticate requests (optional bearer token)
- Enqueue messages to SQLite queue
- Return immediate 202 response

#### Design Decisions

**Request Body Size Limit** (default 35 MB)
- Prevents DoS attacks from large uploads
- Configurable via `max_body_size_bytes`
- Applied at HTTP layer before parsing

**Authentication**
- Optional bearer token (`Authorization: Bearer <token>`)
- Constant-time comparison prevents timing attacks
- Can be disabled for internal/trusted networks

**Timeouts**
- Read timeout: 30s (configurable)
- Write timeout: 30s (configurable)
- Idle timeout: 120s (configurable)

**Why these defaults**: Balances security (prevents slowloris), performance (allows large messages), and compatibility (works with slow clients).

**TLS/HTTPS Support**
- Optional TLS encryption via `tls_enabled = true`
- Requires certificate and private key files
- Supports Let's Encrypt certificates
- Server automatically uses HTTPS when enabled

```toml
[http]
tls_enabled = true
tls_cert_file = "/etc/letsencrypt/live/example.com/fullchain.pem"
tls_key_file = "/etc/letsencrypt/live/example.com/privkey.pem"
```

**Why optional**: Internal/trusted networks may not need TLS overhead. Production deployments should enable TLS for security.

#### Message Validation

```go
// Required fields
- from_address (email format)
- to_address (email format)
- subject (not empty)
- raw_message (RFC 822 format)

// Automatic generation
- message_id (cryptographically random, 20 chars)
- enqueued_at (RFC3339 timestamp)
- expires_at (enqueued_at + 48 hours)
```

**Why validate early**: Fail fast before persistence. Easier to return HTTP errors than handle invalid messages in workers.

---

### Queue System

**Location**: `internal/queue/queue.go`

#### SQLite Schema

```sql
CREATE TABLE queue (
  message_id TEXT PRIMARY KEY,
  status TEXT NOT NULL,
  from_address TEXT NOT NULL,
  to_address TEXT NOT NULL,
  to_domain TEXT NOT NULL,
  subject TEXT NOT NULL,
  raw_message TEXT NOT NULL,
  enqueued_at TEXT NOT NULL,
  next_retry_at TEXT,
  expires_at TEXT NOT NULL,
  attempts INTEGER DEFAULT 0,
  last_error TEXT,
  last_smtp_code INTEGER,
  last_smtp_response TEXT,
  delivered_at TEXT,
  delivered_mx TEXT,
  delivered_source_ip TEXT,
  delivered_smtp_code INTEGER,
  delivered_smtp_response TEXT
);

CREATE INDEX idx_status_next_retry ON queue(status, next_retry_at);
CREATE INDEX idx_to_domain ON queue(to_domain);
CREATE INDEX idx_enqueued_at ON queue(enqueued_at);
CREATE INDEX idx_expires_at ON queue(expires_at);
```

#### Design Decisions

**WAL Mode**
```go
PRAGMA journal_mode=WAL;
PRAGMA synchronous=NORMAL;
PRAGMA busy_timeout=5000;
```

- **WAL**: Allows concurrent reads during writes
- **synchronous=NORMAL**: Balances durability and performance
- **busy_timeout**: Prevents immediate lock failures

**Connection Pool**
```go
SetMaxOpenConns(10)  // Multiple concurrent readers
SetMaxIdleConns(5)   // Keep connections warm
writeMu sync.Mutex   // Serialize all writes
```

**Why**: WAL supports 1 writer + N readers. We serialize writes with a mutex and allow concurrent reads by increasing the connection pool from 1 to 10.

**Event Notification**
```go
notifyCh chan struct{}         // Notify workers of new messages
callbackNotifyCh chan struct{} // Notify callback handler
```

After any write (enqueue, retry schedule, callback), a non-blocking send notifies listeners.

**Why channels over polling**: Instant notification reduces delivery latency from "up to 30s" to "milliseconds".

#### Message States

```go
const (
  StatusPending    = "pending"      // Ready for delivery
  StatusProcessing = "processing"   // Worker actively delivering
  StatusDelivered  = "delivered"    // Successfully delivered
  StatusFailed     = "failed"       // Retrying after temporary failure
  StatusHardBounce = "hard_bounce"  // Permanent failure (5xx)
  StatusExpired    = "expired"      // Exceeded max_message_age_hours
)
```

**State Transitions**:
```
pending → processing → delivered
                    → failed → processing (retry)
                    → hard_bounce
                    → expired
```

---

### Worker Pool

**Location**: `internal/worker/worker.go`

#### Architecture

```go
type Worker struct {
  queue           *queue.Queue
  deliverer       *delivery.Deliverer
  retryScheduler  *delivery.RetryScheduler
  callbackHandler *callback.CallbackHandler
  config          *config.QueueConfig
  deliveryConfig  *config.DeliveryConfig
  workers         []*workerInstance
  ctx             context.Context
  cancel          context.CancelFunc
}
```

#### Design Decisions

**Worker Count** (default: 10)
- Each worker is an independent goroutine
- Workers share the same queue and deliverer
- Configurable based on available resources

**Batch Processing**
```go
// Each iteration:
1. Fetch batch_size messages (default: 5)
2. Process each message sequentially
3. Wait for notification or poll interval
```

**Why sequential in batch**: SMTP connections have inherent serialization. Parallel delivery within a worker doesn't improve throughput and complicates error handling.

**Event-Driven with Fallback**
```go
select {
  case <-notifyChan:           // Instant notification
  case <-time.After(30*time.Second): // Fallback poll
  case <-ctx.Done():           // Graceful shutdown
}
```

**Why hybrid**: Events provide instant response, polling ensures messages aren't stuck if notification is missed (channel is non-blocking).

#### Graceful Shutdown

```go
1. context.Cancel() signals all workers
2. In-flight deliveries check ctx.Done()
3. Wait for workers to finish current message
4. No new messages are fetched
```

**Why**: Prevents message loss during deploys. In-flight deliveries complete or are re-queued.

---

### Delivery Engine

**Location**: `internal/delivery/delivery.go`

#### Core Flow

```go
DeliverMessage(ctx, msg):
  1. Check destination throttle (rate limiting)
  2. Record attempt timestamp
  3. Lookup MX records (with caching)
  4. Select source IP (round-robin/random/hash)
  5. Try each MX in priority order:
     a. Resolve MX hostname to IPs
     b. Separate IPv6 and IPv4 addresses
     c. Try up to max_ips_per_mx (default: 5):
        - Try all IPv6 first
        - Then try all IPv4
     d. Stop on success or permanent error
     e. Continue to next MX on temporary error
  6. Return DeliveryResult
```

#### Design Decisions

**IPv6-First Delivery**

```go
// For each MX, try:
1. All IPv6 addresses (tcp6)
2. All IPv4 addresses (tcp4)
```

**Why**:
- RFC 8305 (Happy Eyeballs v2) recommends IPv6 preference
- Major mail providers (Gmail, Outlook) prefer IPv6
- Better deliverability in IPv6-native environments

**Multihomed MX Handling**

When an MX hostname resolves to multiple IPs (e.g., `gmail-smtp-in.l.google.com` → [142.250.27.26, 142.250.27.27, 2607:f8b0:4001:c03::1a]):

```go
// OLD (buggy): DialContext(mx.Host) only tried first IP
// NEW (correct): Explicitly resolve, try all IPs
addrs := resolver.LookupHost(ctx, mxHost)
for _, ip := range addrs {
  conn := DialContext(ctx, "tcp6", ip+":25")
}
```

**Why**: Many large mail servers are multihomed for redundancy. Trying all IPs before moving to next MX priority improves deliverability.

**IP Limit Protection** (default: 5 IPs per MX)

```go
maxIPs := config.MaxIPsPerMX  // default: 5
totalAttempts := 0

for _, ip := range ips {
  if totalAttempts >= maxIPs { break }
  // try delivery
  totalAttempts++
}
```

**Why**: Malicious MX records could return hundreds of IPs. Limiting prevents abuse while allowing legitimate multihomed servers.

**Destination Throttling** (default: 2 seconds between deliveries)

```go
type DestinationThrottle struct {
  mu           sync.RWMutex
  lastAttempts map[string]time.Time  // domain → last attempt
  minInterval  time.Duration
}
```

**Why anti-spam behavior**:
- Prevents rapid-fire connections to same domain
- If 5 messages to gmail.com are queued, they deliver 2+ seconds apart
- Looks like legitimate mail server, not bulk spammer
- Configurable per deployment needs

**Source IP Selection**

Three strategies (configurable):

1. **Round-robin** (default): Cycle through IPs sequentially
2. **Random**: Random IP per message
3. **Hash-domain**: Consistent IP per recipient domain

```go
type IPRotator struct {
  ips       []string
  strategy  string
  counter   uint64  // atomic counter for round-robin
}
```

**Why multiple strategies**:
- Round-robin: Even IP reputation distribution
- Random: Unpredictable for debugging
- Hash-domain: Consistent IP improves SPF/DKIM reputation per domain

**Source IP Binding**

```go
sourceIP := net.ParseIP(sourceIPStr)
if sourceIP.To4() != nil {
  // IPv4 source requires tcp4 connection
  if network == "tcp6" { error }
}
```

**Why validate**: Can't bind IPv4 source to IPv6 connection and vice versa. Prevents cryptic network errors.

---

### DNS Resolver

**Location**: `internal/delivery/dns_resolver.go`, `internal/delivery/mx_lookup.go`

#### Custom DNS Resolution

```go
type DNSResolver struct {
  resolvers []string  // Custom DNS servers (optional)
  timeout   time.Duration
  logger    *zap.Logger
}
```

#### Design Decisions

**Custom DNS Servers** (optional)

```toml
dns_resolvers = ["8.8.8.8:53", "[2001:4860:4860::8888]:53"]
```

**Why**:
- Corporate networks may have restrictive DNS
- Public resolvers (Google, Cloudflare) are often more reliable
- IPv6 DNS servers supported

**UDP-to-TCP Fallback**

```go
// Try UDP first (fast, low overhead)
conn, err := DialContext(ctx, "udp", resolver)
if err != nil {
  // Fallback to TCP (reliable, no size limit)
  conn, err = DialContext(ctx, "tcp", resolver)
}
```

**Why**:
- UDP is faster for small responses
- TCP handles truncated responses (>512 bytes)
- Some firewalls block UDP/53
- Large MX responses can exceed UDP limit

**Automatic AAAA + A Queries**

```go
addrs, _ := resolver.LookupHost(ctx, hostname)
// Returns both IPv6 (AAAA) and IPv4 (A) records
```

**Why**: Go's `LookupHost` automatically queries both record types, providing IPv6 and IPv4 addresses in one call.

#### MX Caching

```sql
CREATE TABLE mx_cache (
  domain TEXT PRIMARY KEY,
  mx_records TEXT NOT NULL,  -- JSON array
  cached_at TEXT NOT NULL,
  expires_at TEXT NOT NULL
);
```

**Cache Strategy**:
- Successful lookups: cached for `mx_cache_ttl_seconds` (default: 3600s / 1 hour)
- Failed lookups (NXDOMAIN): cached for `dns_cache_negative_ttl` (default: 60s)

**Why cache**:
- Reduces DNS query load (esp. for bulk sending)
- Improves delivery latency
- Respects DNS TTL while preventing excessive queries

**Cache Invalidation**:
```go
// Automatic: Check expires_at on every lookup
// Manual: CleanupExpiredMXCache() removes old entries
// Per-domain: InvalidateMXCache(domain) for specific domain
```

---

### Callback System

**Location**: `internal/callback/callback.go`

#### Callback Types

```go
type EventType string

const (
  EventDelivered  = "delivered"   // 2xx SMTP response
  EventFailed     = "failed"      // Temporary failure, will retry
  EventBounced    = "bounced"     // Permanent failure (5xx)
  EventExpired    = "expired"     // Exceeded max_message_age_hours
)
```

#### Callback Payload

```json
{
  "event_type": "delivered",
  "message_id": "msg_abc123...",
  "from_address": "sender@example.com",
  "to_address": "recipient@example.com",
  "timestamp": "2025-10-05T18:42:31+02:00",
  "smtp_code": 250,
  "smtp_response": "OK",
  "mx_host": "mx1.example.com",
  "source_ip": "192.168.1.100",
  "attempts": 1,
  "error_message": null
}
```

#### Design Decisions

**Separate Callback Queue**

```sql
CREATE TABLE callbacks (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  message_id TEXT NOT NULL,
  event_type TEXT NOT NULL,
  payload TEXT NOT NULL,  -- JSON
  created_at TEXT NOT NULL,
  attempts INTEGER DEFAULT 0,
  next_retry_at TEXT,
  completed_at TEXT
);
```

**Why separate table**: Callbacks can fail independently of delivery. Separate queue allows different retry logic and prevents blocking delivery queue.

**Callback Retry Logic**

```go
max_retries: 5
retry_delay: 30 seconds (fixed, not exponential)
```

**Why fixed delay**: Webhooks usually fail due to temporary network issues or service restarts. Fixed retry is simpler and sufficient.

**Batch Processing** (default: 10 callbacks per iteration)

```go
callbacks := q.FetchPendingCallbacks(batchSize)
for _, cb := range callbacks {
  select {
    case <-ctx.Done(): return  // Context check
    default: sendCallback(cb)
  }
}
```

**Why batch**: Reduces database queries and allows efficient processing.

**HTTP Request with Context**

```go
req, _ := http.NewRequestWithContext(ctx, "POST", webhookURL, payload)
req.Header.Set("Content-Type", "application/json")
req.Header.Set("Authorization", "Bearer "+authToken)

client := &http.Client{Timeout: 10 * time.Second}
resp, _ := client.Do(req)
```

**Why context**: Allows cancellation during shutdown. Prevents hanging HTTP requests.

**Authentication**

```toml
[callbacks]
webhook_url = "https://example.com/webhook"
auth_token = "secret"  # Sent as Authorization: Bearer <token>
```

**Why bearer token**: Simple, stateless auth suitable for webhooks. Receiving service validates token.

---

### Configuration

**Location**: `internal/config/config.go`

#### Configuration File Format (TOML)

```toml
[http]
listen = ":8080"
auth_token = "secret"
max_body_size_bytes = 36700160  # 35 MB
read_timeout_seconds = 30
write_timeout_seconds = 30
idle_timeout_seconds = 120

[queue]
database_path = "./queue.db"
worker_count = 10
batch_size = 5
cleanup_interval_seconds = 60
poll_interval_seconds = 30

[delivery]
source_ips = ["192.168.1.100", "2001:db8::1"]
ip_selection = "round-robin"
mx_cache_ttl_seconds = 3600
connection_timeout_seconds = 30
smtp_timeout_seconds = 60
max_ips_per_mx = 5
min_delivery_interval_seconds = 2
throttle_retry_delay_seconds = 5

# DNS
dns_resolvers = ["8.8.8.8:53", "[2001:4860:4860::8888]:53"]
dns_timeout_seconds = 5
dns_cache_negative_ttl = 60

# Retry
max_message_age_hours = 48
initial_retry_delay_seconds = 300      # 5 minutes
max_retry_delay_seconds = 43200        # 12 hours
backoff_multiplier = 2.0
greylist_retry_delay_seconds = 120     # 2 minutes

[callbacks]
webhook_url = "https://example.com/webhook"
auth_token = "secret"
timeout_seconds = 10
max_retries = 5
retry_delay_seconds = 30
batch_size = 10
```

#### Design Decisions

**TOML over JSON/YAML**
- More readable than JSON
- Simpler than YAML (no indentation issues)
- Native Go support via `github.com/BurntSushi/toml`

**Defaults via SetDefaults()**

```go
func (c *Config) SetDefaults() {
  if c.Queue.WorkerCount == 0 {
    c.Queue.WorkerCount = 10
  }
  // ... etc
}
```

**Why**: Allows minimal config files. Only specify what differs from defaults.

**No Environment Variables**

Configuration is file-only (no env var overrides).

**Why**: Simpler deployment. Config file can be version-controlled. For secrets in production, use secret management (Vault, AWS Secrets Manager) to generate config file.

---

## Security Features

### 1. **TLS/HTTPS Support**

**Configuration**
```toml
[http]
tls_enabled = true
tls_cert_file = "/path/to/cert.pem"
tls_key_file = "/path/to/key.pem"
```

**Implementation**: Uses Go's `http.Server.ListenAndServeTLS()` with provided certificate and key files.

**Why**: Encrypts HTTP traffic to prevent eavesdropping and man-in-the-middle attacks. Essential for production deployments.

**Certificate options**:
- **Let's Encrypt**: Free automated certificates (recommended)
- **Self-signed**: For testing/development only
- **Commercial CA**: For enterprise requirements

**Validation**: Server validates that cert and key files are specified when `tls_enabled = true`, failing fast on startup if missing.

### 2. **Authentication**

**Constant-Time Comparison**
```go
import "crypto/subtle"

expectedToken := []byte(config.AuthToken)
providedToken := []byte(extractedToken)

if subtle.ConstantTimeCompare(expectedToken, providedToken) != 1 {
  return errors.New("invalid token")
}
```

**Why**: Prevents timing attacks. Variable-time comparison (`==`) leaks information through execution time.

### 3. **Request Body Size Limits**

```go
http.MaxBytesReader(w, r.Body, maxBodySizeBytes)
```

**Why**: Prevents memory exhaustion from gigabyte-sized uploads.

### 4. **Message ID Generation**

```go
import "crypto/rand"

func GenerateMessageID() string {
  b := make([]byte, 15)
  rand.Read(b)  // Cryptographically secure
  return "msg_" + base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b)
}
```

**Why crypto/rand over math/rand**: Prevents message ID prediction. Math/rand is deterministic and unsuitable for security-sensitive IDs.

### 5. **SQL Injection Prevention**

All queries use parameterized statements:
```go
db.Exec("INSERT INTO queue (message_id, ...) VALUES (?, ...)", msg.MessageID, ...)
// Never: db.Exec("INSERT ... VALUES ('" + msg.MessageID + "')")
```

**Why**: Prepared statements prevent SQL injection. User input never concatenated into queries.

### 6. **TLS for SMTP**

```go
// Opportunistic STARTTLS
if err := client.StartTLS(&tls.Config{
  ServerName: mxHost,
}); err != nil {
  // Log but continue (not all servers support TLS)
}
```

**Why opportunistic**: RFC 3207 specifies STARTTLS as optional. Some legacy servers don't support it. We try TLS but fall back to plaintext if unavailable.

### 7. **Context Timeouts**

All network operations have timeouts:
```go
ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
defer cancel()
conn, err := dialer.DialContext(ctx, "tcp", addr)
```

**Why**: Prevents indefinite hangs from unresponsive servers.

---

## Error Handling & Retry Logic

### Error Classification

**Location**: `internal/delivery/error_classifier.go`

```go
type ErrorCategory string

const (
  ErrorTemporary  // Retry with exponential backoff
  ErrorPermanent  // Hard bounce, don't retry
  ErrorGreylist   // Aggressive retry (421 responses)
  ErrorNetwork    // DNS/connection failures, retry
  ErrorThrottled  // Rate limited, quick retry
)
```

#### Classification Logic

**SMTP Response Codes**:
```go
2xx → Success (not an error)
421 → ErrorGreylist (retry in 2 minutes)
4xx → ErrorTemporary (exponential backoff)
5xx → ErrorPermanent (hard bounce)
```

**Network Errors**:
```go
"no such host", "dns", "lookup" → ErrorNetwork
"connection refused", "timeout" → ErrorNetwork
"tls", "certificate", "handshake" → ErrorNetwork
```

**Throttle Errors**:
```go
Destination rate limit triggered → ErrorThrottled
```

### Retry Scheduling

**Location**: `internal/delivery/retry.go`

```go
type RetryScheduler struct {
  initialDelay  time.Duration  // 5 minutes
  maxDelay      time.Duration  // 12 hours
  multiplier    float64        // 2.0
  greylistDelay time.Duration  // 2 minutes
}
```

#### Retry Calculation

**Exponential Backoff**:
```go
delay := initialDelay * (multiplier ^ attempts)
if delay > maxDelay {
  delay = maxDelay
}
```

Example timeline:
```
Attempt 1: 5 minutes
Attempt 2: 10 minutes
Attempt 3: 20 minutes
Attempt 4: 40 minutes
Attempt 5: 80 minutes
Attempt 6: 160 minutes
Attempt 7+: 720 minutes (12 hours - capped)
```

**Greylisting** (421 responses):
```go
// Fixed 2-minute retry regardless of attempts
delay := 2 * time.Minute
```

**Why**: Greylisting expects retry within minutes, not hours. Exponential backoff defeats the purpose.

**Throttled Deliveries**:
```go
// Fixed 5-second retry, doesn't count as attempt
delay := 5 * time.Second
attempts = attempts  // Don't increment
```

**Why**: Rate limiting is artificial (self-imposed), not a delivery failure. Quick retry once throttle window passes.

### Message Expiration

```go
max_message_age_hours = 48  // Default

if time.Now().After(msg.ExpiresAt) {
  status = StatusExpired
  // Send "expired" callback
  // Delete from queue
}
```

**Why 48 hours**: RFC 5321 recommends 4-5 days, but modern environments expect faster feedback. 48 hours balances retry attempts with timely bounce notifications.

---

## Circuit Breaker & Failover

### Circuit Breaker Pattern

**Purpose**: Automatically stop accepting HTTP requests when local delivery infrastructure fails, preventing queue buildup and providing fast-fail behavior.

**How it works**:
```
1. Closed (normal): All requests accepted, deliveries attempted
2. Open (failing): Requests rejected with 503, deliveries paused
3. Half-Open (testing): Limited requests to test recovery
```

**Configuration**:
```toml
[delivery]
circuit_breaker_enabled = true            # Enable (recommended)
circuit_breaker_failure_threshold = 5     # Failures before opening
circuit_breaker_success_threshold = 2     # Successes to close
circuit_breaker_open_timeout_seconds = 60 # Wait before testing
```

**Trigger conditions**:
- Network errors (connection refused, timeouts)
- DNS failures (NXDOMAIN, resolver unreachable)
- Local IP binding failures

**NOT triggered by**:
- Remote SMTP errors (4xx, 5xx codes)
- Greylisting (421)
- Rate limiting (our own throttling)

**Why**: Prevents cascading failures. If your network/DNS is down, rejecting requests immediately is better than queueing messages that can't be delivered.

**HTTP Response when open**:
```json
{
  "error": "Service temporarily unavailable due to delivery failures",
  "retry_after": "60"
}
```

### Source IP Failover

**Automatic rotation on local failures**:
```
1. Try delivery from source IP #1
2. If local network error → Try source IP #2
3. If local network error → Try source IP #3
4. If remote SMTP error → Move to next MX (don't rotate IPs)
```

**Configuration**:
```toml
source_ips = ["192.168.1.100", "192.168.1.101", "192.168.1.102"]
```

**Why**: If one local IP has network issues (routing, firewall, interface down), automatically fail over to another IP. Improves reliability without manual intervention.

**Example scenario**:
```
Message to gmail.com
├─ MX: gmail-smtp-in.l.google.com
│  ├─ Try from 192.168.1.100 → Connection refused (local error)
│  ├─ Try from 192.168.1.101 → Success ✓
│  └─ (Circuit breaker: +1 failure, then +1 success)
```

---

## IP Reputation Tracking

### Overview

Fune includes an intelligent IP reputation tracking system that automatically detects and manages source IPs with poor reputation or blacklist issues. When a delivery fails due to reputation problems, the affected IP is temporarily removed from the rotation pool, preventing further failures and automatically retried after a configurable period.

**Location**: `internal/delivery/ip_reputation.go`

### How It Works

```
1. Normal delivery attempt from IP 192.168.1.100
2. Remote server rejects: "550 blocked by Spamhaus"
3. Error classified as ErrorReputation
4. IP 192.168.1.100 marked as "degraded"
5. Alert sent to configured webhook
6. IP removed from rotation pool
7. Subsequent deliveries use other healthy IPs
8. After 48 hours (configurable), IP is retried
9. If successful → IP marked "recovered", alert sent
10. If failed → IP remains degraded for another 48 hours
```

### IP States

```go
type IPState string

const (
  IPStateHealthy  IPState = "healthy"   // Normal operation
  IPStateDegraded IPState = "degraded"  // Temporarily removed from pool
)
```

**State Transitions**:
```
healthy → degraded (on reputation error)
        ← recovered (on successful delivery after retry time)
        → degraded (if retry fails)
```

### Configuration

```toml
[reputation]
# IP reputation tracking and alerting
enable_ip_tracking = true              # Enable IP reputation tracking (default: true)
alert_webhook_url = "https://example.com/api/reputation-alert"
alert_auth_token = "secret-token"      # Optional bearer token
alert_timeout_seconds = 10             # Webhook timeout (default: 10)
degraded_retry_hours = 48              # Hours before retrying (default: 48)
degraded_ip_cleanup_hours = 168        # History retention in hours - 7 days (default: 168)
```

### Reputation Alert Webhook

When an IP is degraded or recovered, a JSON POST request is sent to the configured webhook:

```json
{
  "timestamp": "2025-10-06T10:30:00Z",
  "source_ip": "192.168.1.100",
  "event_type": "degraded",              // or "recovered"
  "from": "sender@example.com",
  "to": "recipient@example.com",
  "subject": "Important Message",
  "idempotency_key": "abc123",           // if provided
  "smtp_code": 550,
  "smtp_response": "IP blocked by Spamhaus RBL",
  "mx_host": "mx.example.com",
  "retry_after": "2025-10-08T10:30:00Z",
  "degraded_ips_count": 1                // Total degraded IPs
}
```

**HTTP Headers**:
```
Content-Type: application/json
Authorization: Bearer <alert_auth_token>  // if configured
```

**Response Codes**:
- `200-299`: Alert received successfully
- `4xx/5xx`: Alert failed (logged but doesn't affect delivery)

### Error Classification

The system identifies reputation issues by analyzing SMTP responses for specific keywords:

```go
reputationKeywords := []string{
  "blocked",
  "blacklist",
  "poor reputation",
  "rejected for policy reasons",
  "rbl",           // Real-time Blackhole List
  "dnsbl",         // DNS-based Blacklist
  "spamhaus",      // Spamhaus blacklist
  "proofpoint",    // Proofpoint filters
  "cloudmark",     // Cloudmark reputation
  "barracuda",     // Barracuda filters
}
```

**Example SMTP responses triggering degradation**:
- `550 5.7.1 IP blocked by Spamhaus`
- `554 rejected for poor reputation`
- `550 Your IP is on the RBL list`
- `554 Connection refused due to DNSBL match`

**Not triggered by**:
- User not found errors (550)
- Mailbox full errors (552)
- Rate limiting (421, 450)
- Generic temporary failures (4xx)

### Delivery Flow with Reputation Tracking

```go
DeliverMessage(ctx, msg):
  1. Get all configured source IPs
  2. Filter out degraded IPs
  3. If all IPs degraded:
     → Log warning
     → Fall back to system default IP (no binding)
  4. Select from healthy IPs using strategy (round-robin/random/hash)
  5. Attempt delivery
  6. Record delivery result:
     - Success + IP was degraded → Mark recovered, send alert
     - Failure + Reputation error → Mark degraded, send alert
     - Failure + Other error → No reputation change
```

### Automatic IP Recovery

**Retry Schedule**:
```
IP degraded at:    2025-10-06 10:00
Retry after:       2025-10-08 10:00 (48 hours later)
Status:           "degraded" but eligible for retry
Action:           IP included in selection pool
Result if success: IP marked "recovered", alert sent
Result if failure: Retry after pushed forward another 48 hours
```

**Why 48 hours**:
- Blacklists typically update within 24-48 hours
- Allows time for reputation to improve
- Prevents rapid retry loops

### Cleanup & Maintenance

**Automatic Cleanup**:
```go
// Runs hourly via background job
tracker.Cleanup()
```

Removes degraded IP entries that are:
- Older than `degraded_ip_cleanup_hours` (default: 7 days)
- AND past their retry time

**Why cleanup**: Prevents unbounded memory growth while maintaining recent history for debugging.

### Fallback Behavior

**All IPs Degraded Scenario**:
```
Configured IPs: [192.168.1.100, 192.168.1.101, 192.168.1.102]
Degraded IPs:   [192.168.1.100, 192.168.1.101, 192.168.1.102]
Healthy IPs:    [] (empty)

Action: Fall back to system default IP (no source binding)
Log:    "all source IPs are degraded, using default"
```

**Why**: Ensures deliveries can continue even when all configured IPs have issues. System default IP may have different reputation.

### Thread Safety

The reputation tracker is thread-safe and supports concurrent access:

```go
type IPReputationTracker struct {
  mu          sync.RWMutex
  degradedIPs map[string]*DegradedIPInfo
  // ... other fields
}
```

**Concurrent operations**:
- Multiple workers can check IP health simultaneously (read lock)
- IP degradation/recovery operations are serialized (write lock)
- Webhook alerts sent in background goroutines (non-blocking)

### Disabling Reputation Tracking

To disable IP reputation tracking:

```toml
[reputation]
enable_ip_tracking = false
```

**When disabled**:
- All IPs always considered healthy
- No degradation or recovery tracking
- No webhook alerts sent
- Zero performance overhead

**Use cases**:
- Testing/development environments
- Single IP deployments
- When using external reputation management

### Integration with Source IP Rotation

Reputation tracking integrates seamlessly with IP rotation strategies:

```toml
[delivery]
source_ips = ["192.168.1.100", "192.168.1.101", "192.168.1.102"]
ip_selection = "round-robin"  # or "random", "hash-domain"

[reputation]
enable_ip_tracking = true
```

**Flow**:
1. IP rotator provides all configured IPs
2. Reputation tracker filters out degraded IPs
3. Rotation strategy applied to healthy IPs only

**Example**:
```
All IPs:     [.100, .101, .102]
Degraded:    [.101]
Healthy:     [.100, .102]
Round-robin: .100 → .102 → .100 → .102 (skips .101)
```

### Monitoring & Observability

**Prometheus Metrics**:
```promql
# IP reputation status (1=degraded, 0=healthy)
fune_ip_reputation_degraded{source_ip="192.168.1.100"}

# Total degradation/recovery events
fune_ip_reputation_events_total{event_type="degraded",source_ip="192.168.1.100"}
fune_ip_reputation_events_total{event_type="recovered",source_ip="192.168.1.100"}
```

**Example Queries**:
```promql
# Count currently degraded IPs
count(fune_ip_reputation_degraded == 1)

# List degraded IPs
fune_ip_reputation_degraded == 1

# Rate of degradation events per hour
rate(fune_ip_reputation_events_total{event_type="degraded"}[1h])
```

**Recommended Alerts**:
- `SourceIPDegraded`: Alert when any IP becomes degraded
- `MultipleIPsDegraded`: Alert when 2+ IPs are degraded
- `FrequentIPDegradation`: Alert on high degradation rate

See [Observability & Metrics](#observability--metrics) section for complete Prometheus documentation.

**Degraded IP Status**:
```go
tracker.GetDegradedIPs()  // Returns map[IP]*DegradedIPInfo

type DegradedIPInfo struct {
  IP               string
  DegradedAt       time.Time
  RetryAfter       time.Time
  FailureCount     int
  LastFailureError string
  LastSMTPCode     int
  LastSMTPResponse string
}
```

**Logging**:
```
INFO  IP marked as degraded due to reputation failure
      ip=192.168.1.100 smtp_code=550 retry_after=2025-10-08T10:30:00Z

INFO  IP recovered from degraded state
      ip=192.168.1.100 degraded_duration=48h5m total_failures=3

WARN  all source IPs are degraded, using default
      message_id=msg_abc123 total_ips=3
```

### Performance Characteristics

**Memory Usage**:
- ~200 bytes per degraded IP entry
- Bounded by number of configured source IPs
- Automatic cleanup prevents growth

**CPU Overhead**:
- Negligible for IP health checks (map lookup)
- Webhook alerts sent in background goroutines
- No impact on delivery latency

**Network**:
- One HTTP POST per degradation/recovery event
- Non-blocking (failures logged but don't affect delivery)

### Testing

**Comprehensive test coverage** (24 tests):
- IP lifecycle: healthy → degraded → recovered
- Webhook alert validation (payload, headers, auth)
- Thread safety (100 concurrent goroutines)
- Edge cases (empty IPs, all degraded, disabled tracking)
- Integration with delivery system

See `internal/delivery/ip_reputation_test.go` for complete test suite.

### Design Decisions

**Why track reputation separately from circuit breaker?**
- Circuit breaker: Local network failures (affects all traffic)
- Reputation tracker: Remote reputation issues (IP-specific)
- Different failure modes, different solutions

**Why 48-hour default retry?**
- Most blacklists update within 24-48 hours
- Balances recovery time with avoiding rapid retries
- Can be configured per deployment needs

**Why webhook alerts?**
- Real-time notification of reputation issues
- Enables external monitoring/alerting
- Allows integration with ticketing systems
- Provides delivery context (from, to, subject)

**Why filter before rotation?**
- Simpler logic: rotation strategy unaware of reputation
- Clean separation of concerns
- Easy to test independently

**Why automatic recovery?**
- Reduces manual intervention
- IPs can recover naturally over time
- Alert on recovery provides visibility

---

## Anti-Spam Measures

### 1. **Destination Rate Limiting**

```go
minInterval := 2 * time.Second  // Configurable

if lastAttempt[domain] + minInterval > now {
  return ErrorThrottled
}
lastAttempt[domain] = now
```

**Why**: Prevents rapid-fire connections to same domain. Spacing deliveries by 2+ seconds looks like legitimate mail server behavior, not bulk spam.

### 2. **IP Limit Per MX**

```go
maxIPsPerMX := 5  // Configurable

// Try up to 5 IPs per MX, then move to next MX
```

**Why**: Malicious MX records could return hundreds of IPs to waste resources. Limiting prevents abuse.

### 3. **Message Age Expiration**

```go
maxAge := 48 * time.Hour

if now - msg.EnqueuedAt > maxAge {
  delete(msg)
}
```

**Why**: Prevents infinite retries. Old messages likely invalid/unwanted.

### 4. **Connection Timeouts**

```go
connectionTimeout := 30 * time.Second
smtpTimeout := 60 * time.Second
```

**Why**: Prevents tarpitting attacks where malicious servers keep connections open indefinitely.

### 5. **Exponential Backoff**

Increasing delays between retries prevent hammering failing servers.

**Why**: Aggressive retries can trigger anti-spam measures at receiving servers. Exponential backoff is standard email behavior.

---

## Observability & Metrics

Fune exposes Prometheus metrics for comprehensive monitoring and alerting.

### Metrics Endpoint

**URL**: `/metrics` (configurable via `metrics_path`)
**Format**: Prometheus text format
**Authentication**: None (handle via reverse proxy if needed)

### Available Metrics

#### 1. Queue Depth Metrics

```promql
# Number of messages in queue by status
fune_queue_depth{status="queued"}
fune_queue_depth{status="sending"}
fune_queue_depth{status="delivered"}
fune_queue_depth{status="hard_bounce"}
fune_queue_depth{status="temp_expired"}
fune_queue_depth{status="expired"}
```

**Use cases**:
- Alert on growing queued backlog
- Track delivery success rate
- Monitor bounce rates

#### 2. Delivery Metrics

```promql
# Total delivery attempts by outcome
fune_delivery_attempts_total{outcome="success"}
fune_delivery_attempts_total{outcome="permanent_error"}
fune_delivery_attempts_total{outcome="temporary_error"}
fune_delivery_attempts_total{outcome="network_error"}
fune_delivery_attempts_total{outcome="throttled"}

# Delivery duration histogram (seconds)
fune_delivery_duration_seconds_bucket{outcome="success",le="1.0"}
fune_delivery_duration_seconds_sum{outcome="success"}
fune_delivery_duration_seconds_count{outcome="success"}
```

**Use cases**:
- Calculate success rate: `rate(fune_delivery_attempts_total{outcome="success"}[5m])`
- Alert on network errors indicating infrastructure issues
- Track delivery latency percentiles

#### 3. Callback Metrics

```promql
# Webhook callback attempts by outcome and event type
fune_callback_attempts_total{outcome="success",event_type="delivered"}
fune_callback_attempts_total{outcome="failure",event_type="bounced"}

# Callback duration histogram (seconds)
fune_callback_duration_seconds_bucket{outcome="success",le="0.5"}
fune_callback_duration_seconds_sum{outcome="success"}
fune_callback_duration_seconds_count{outcome="success"}
```

**Use cases**:
- Monitor webhook delivery success rate
- Alert on slow/failing webhook endpoint
- Track callback latency

#### 4. HTTP Request Metrics

```promql
# HTTP requests by method, path, and status code
fune_http_requests_total{method="POST",path="/",status="202"}
fune_http_requests_total{method="GET",path="/metrics",status="200"}

# HTTP request duration histogram (seconds)
fune_http_request_duration_seconds_bucket{method="POST",path="/",le="0.01"}
fune_http_request_duration_seconds_sum{method="POST",path="/"}
fune_http_request_duration_seconds_count{method="POST",path="/"}
```

**Use cases**:
- Monitor API latency
- Track request volume
- Alert on 5xx errors

#### 5. Circuit Breaker Metrics

```promql
# Current circuit breaker state (0=closed, 1=half-open, 2=open)
fune_circuit_breaker_state

# Circuit breaker state transitions
fune_circuit_breaker_transitions_total{from_state="closed",to_state="open"}
fune_circuit_breaker_transitions_total{from_state="open",to_state="half_open"}
fune_circuit_breaker_transitions_total{from_state="half_open",to_state="closed"}
```

**Use cases**:
- Alert when circuit breaker opens (infrastructure failure)
- Track how often circuit breaker activates
- Monitor recovery patterns

#### 6. IP Reputation Metrics

```promql
# IP reputation status (1=degraded, 0=healthy) by source IP
fune_ip_reputation_degraded{source_ip="192.168.1.100"}
fune_ip_reputation_degraded{source_ip="192.168.1.101"}

# Total IP reputation events by event type and source IP
fune_ip_reputation_events_total{event_type="degraded",source_ip="192.168.1.100"}
fune_ip_reputation_events_total{event_type="recovered",source_ip="192.168.1.100"}
```

**Use cases**:
- Monitor which source IPs are currently degraded
- Alert when an IP becomes degraded due to blacklisting
- Track IP degradation/recovery frequency
- Identify IPs with persistent reputation issues

### Example Prometheus Queries

```promql
# Delivery success rate (5-minute window)
sum(rate(fune_delivery_attempts_total{outcome="success"}[5m]))
/
sum(rate(fune_delivery_attempts_total[5m]))

# 95th percentile delivery latency
histogram_quantile(0.95,
  rate(fune_delivery_duration_seconds_bucket[5m])
)

# Messages waiting in queue
sum(fune_queue_depth{status="queued"})

# Circuit breaker is open (infrastructure down)
fune_circuit_breaker_state == 2

# Network error rate spike
rate(fune_delivery_attempts_total{outcome="network_error"}[5m]) > 0.1

# Number of degraded IPs
count(fune_ip_reputation_degraded == 1)

# Rate of IP degradation events
rate(fune_ip_reputation_events_total{event_type="degraded"}[1h])

# IPs that are currently degraded
fune_ip_reputation_degraded == 1
```

### Recommended Alerts

```yaml
# Prometheus AlertManager rules
groups:
  - name: fune_alerts
    rules:
      - alert: CircuitBreakerOpen
        expr: fune_circuit_breaker_state == 2
        for: 1m
        annotations:
          summary: "Fune circuit breaker is OPEN - delivery infrastructure failing"

      - alert: HighNetworkErrorRate
        expr: rate(fune_delivery_attempts_total{outcome="network_error"}[5m]) > 0.1
        for: 5m
        annotations:
          summary: "High network error rate in delivery attempts"

      - alert: QueueBacklogGrowing
        expr: fune_queue_depth{status="queued"} > 1000
        for: 10m
        annotations:
          summary: "Queue backlog growing - {{ $value }} messages queued"

      - alert: LowDeliverySuccessRate
        expr: |
          sum(rate(fune_delivery_attempts_total{outcome="success"}[5m]))
          /
          sum(rate(fune_delivery_attempts_total[5m])) < 0.8
        for: 15m
        annotations:
          summary: "Delivery success rate below 80%"

      - alert: SourceIPDegraded
        expr: fune_ip_reputation_degraded == 1
        for: 5m
        annotations:
          summary: "Source IP {{ $labels.source_ip }} is degraded due to reputation issues"
          description: "IP has been removed from rotation pool due to blacklisting or poor reputation"

      - alert: MultipleIPsDegraded
        expr: count(fune_ip_reputation_degraded == 1) >= 2
        for: 10m
        annotations:
          summary: "Multiple source IPs are degraded ({{ $value }})"
          description: "Multiple IPs experiencing reputation issues - may indicate systemic problem"

      - alert: FrequentIPDegradation
        expr: rate(fune_ip_reputation_events_total{event_type="degraded"}[6h]) > 0.1
        for: 30m
        annotations:
          summary: "High rate of IP degradation events for {{ $labels.source_ip }}"
          description: "IP is being degraded frequently - investigate reputation issues"
```

### Configuration

```toml
[http]
metrics_enabled = true      # Enable metrics endpoint (default: true)
metrics_path = "/metrics"   # Path for metrics endpoint (default: /metrics)
```

To disable metrics:
```toml
[http]
metrics_enabled = false
```

### Integration with Monitoring Stack

**Prometheus scrape config**:
```yaml
scrape_configs:
  - job_name: 'fune'
    static_configs:
      - targets: ['localhost:8080']
    metrics_path: '/metrics'
    scrape_interval: 15s
```

**Grafana Dashboard**:
- Queue depth over time (graph)
- Delivery success/failure rates (stat)
- Circuit breaker state (stat panel)
- HTTP request latency (heatmap)
- Callback success rate (gauge)

---

## Performance Optimizations

### 1. **SQLite WAL Mode**

```sql
PRAGMA journal_mode=WAL;
```

**Benefit**: 10x more concurrent read operations (1 writer + 10 readers vs 1 connection).

**Trade-off**: Slightly more disk I/O. Acceptable for queue workload.

### 2. **Connection Pool**

```go
db.SetMaxOpenConns(10)   // Was: 1
db.SetMaxIdleConns(5)    // Keep connections warm
```

**Benefit**: Workers can query queue concurrently without lock contention on reads.

**Write serialization**: `writeMu sync.Mutex` ensures only one write at a time (SQLite requirement).

### 3. **MX Caching**

```go
cache[domain] = mxRecords
expiresAt = now + 1 hour
```

**Benefit**:
- Eliminates DNS query for repeated deliveries to same domain
- Reduces delivery latency by ~50-200ms per message

**Trade-off**: MX changes take up to 1 hour to reflect. Acceptable for stability.

### 4. **Event-Driven Workers**

```go
select {
  case <-notifyCh:  // Instant wake-up
  case <-time.After(30*time.Second):  // Fallback poll
}
```

**Benefit**:
- Messages deliver in milliseconds, not "up to 30 seconds"
- Reduces average latency 100x

**Trade-off**: Slightly more complex code. Worth it for responsiveness.

### 5. **Batch Processing**

```go
messages := queue.Fetch(batchSize: 5)
```

**Benefit**: Reduces database round-trips from N queries to 1 query per batch.

**Trade-off**: Larger batches can delay lower-priority messages. Size 5 balances throughput and fairness.

### 6. **Index Strategy**

```sql
CREATE INDEX idx_status_next_retry ON queue(status, next_retry_at);
CREATE INDEX idx_to_domain ON queue(to_domain);
```

**Benefit**:
- `idx_status_next_retry`: Fast pending message queries
- `idx_to_domain`: Fast domain-based queries for debugging

**Trade-off**: Indexes add write overhead. Worth it for read-heavy queue workload.

### 7. **Destination Throttle Cleanup**

```go
// Periodically remove entries older than 1 hour
for domain, lastAttempt := range throttle.lastAttempts {
  if now - lastAttempt > 1*time.Hour {
    delete(throttle.lastAttempts, domain)
  }
}
```

**Benefit**: Prevents unbounded memory growth in long-running processes.

**Trade-off**: Periodic cleanup adds CPU overhead. Runs infrequently (e.g., hourly).

---

## Deployment Considerations

### Horizontal Scaling

**Current**: Single instance (SQLite limits to one writer)

**Future**:
- Partition by domain/hash for multiple instances
- Each instance has independent SQLite database
- Load balancer distributes by hash(recipient_domain)

### Hot Reload (Configuration Updates Without Downtime)

Fune supports hot reload of configuration via **SIGHUP** signal, allowing updates without service restart.

**Reloadable settings**:
- ✅ **Source IPs** - Add/remove delivery IPs dynamically
- ✅ **IP selection strategy** - Switch between round-robin, random, hash-domain
- ✅ **Rate limits** - Update delivery intervals and timeouts
- ✅ **Circuit breaker** - Adjust thresholds, enable/disable
- ✅ **DNS settings** - Change DNS servers, cache TTLs
- ✅ **TLS certificates** - Reload certificates (e.g., after Let's Encrypt renewal)
- ✅ **HTTP timeouts** - Adjust read/write/idle timeouts
- ✅ **Metrics settings** - Enable/disable metrics, change path

**Non-reloadable settings** (require restart):
- ❌ Database path
- ❌ HTTP listen address
- ❌ Worker count
- ❌ Webhook URL

**Usage**:
```bash
# Send SIGHUP to reload configuration
kill -HUP <pid>

# Or with systemd
systemctl reload fune

# Or with Docker
docker kill -s HUP fune-container
```

**TLS Certificate Reload**:
TLS certificates are reloaded automatically on each new connection. No SIGHUP required - just replace the certificate files:
```bash
# Replace cert files (e.g., after Let's Encrypt renewal)
cp /etc/letsencrypt/live/example.com/fullchain.pem /path/to/cert.pem
cp /etc/letsencrypt/live/example.com/privkey.pem /path/to/key.pem

# New connections will use the new certificate immediately
# Existing connections continue with old certificate until they close
```

**Validation**:
Config reload fails if:
- Config file has syntax errors
- Critical fields (database_path, listen, worker_count) changed
- TLS cert/key files are invalid

Failed reload logs error and keeps current configuration.

### Monitoring

**Recommended Metrics**:
- Queue depth (messages in `pending` status)
- Delivery success rate (delivered / total)
- Retry rate (failed / total attempts)
- Average delivery latency
- Callback failure rate
- Circuit breaker state

See [Observability & Metrics](#observability--metrics) section for complete Prometheus metrics.

**Logging**: Structured JSON logs via `zap` for easy parsing/aggregation.

### Backup

**SQLite Database**:
```bash
# Online backup (safe during operation)
sqlite3 queue.db ".backup queue-backup.db"

# Or use WAL checkpoint + file copy
sqlite3 queue.db "PRAGMA wal_checkpoint(TRUNCATE);"
cp queue.db queue-backup.db
```

**Frequency**: Daily backups recommended. Queue is transient (48-hour TTL), but backup prevents data loss on disk failure.

---

## Testing

### Unit Tests

- 14 tests: `internal/callback`
- 5 tests: `internal/config`
- 90 tests: `internal/delivery` (includes 24 IP reputation tests)
- 4 tests: `internal/handler`
- 9 tests: `internal/queue`
- 13 tests: `internal/worker`

**Total**: 135 unit tests

#### IP Reputation Test Coverage

The IP reputation system has comprehensive test coverage (24 tests):

**Basic Functionality** (5 tests):
- Tracker initialization
- Disabled state behavior
- IP health status checks
- Single IP degradation
- Multiple failures on same IP

**Recovery & State Management** (4 tests):
- IP recovery flow
- Automatic recovery on successful delivery
- Reputation error handling
- Non-reputation errors ignored

**IP Filtering & Selection** (3 tests):
- Filtering degraded IPs from pool
- Retry time expiration logic
- Multiple IPs in different states

**Cleanup & Maintenance** (2 tests):
- Old entry cleanup
- Recent entry preservation

**Webhook Alerting** (3 tests):
- Degradation alerts with full payload validation
- Recovery alerts
- Graceful handling when webhook not configured

**Thread Safety & Edge Cases** (3 tests):
- 100 concurrent goroutines
- Defensive copy returns
- Empty IP string handling

**Integration Tests** (4 tests):
- Full workflow with webhook alerts
- All IPs degraded scenario
- Non-reputation errors don't degrade IPs
- Disabled tracking behavior

### Integration Test

`integration_test.go` validates full message flow:
1. HTTP submission
2. Queue persistence
3. MIME construction
4. Expiration calculation
5. Retry scheduling
6. Worker initialization

### Running Tests

```bash
# All tests
go test ./...

# Specific package
go test ./internal/delivery

# With coverage
go test -cover ./...
```

---

## Conclusion

Fune is designed for **reliability**, **performance**, and **operational simplicity**:

- **Reliable**: Persistent queue, exponential backoff, proper retry logic
- **Performant**: Event-driven architecture, connection pooling, caching
- **Simple**: Single binary, SQLite database, TOML configuration
- **Secure**: Constant-time auth, crypto-secure IDs, context timeouts
- **Modern**: IPv6-first, multihomed MX support, destination rate limiting

The architecture balances complexity and capability, providing enterprise-grade SMTP delivery without requiring external dependencies like Redis or RabbitMQ.
