# Fune Component Details

This document provides detailed documentation for each core component in the Fune SMTP delivery system.

## Table of Contents

- [HTTP Handler](#http-handler)
- [Queue System](#queue-system)
- [Worker Pool](#worker-pool)
- [Delivery Engine](#delivery-engine)
- [DNS Resolver](#dns-resolver)
- [Callback System](#callback-system)
- [Configuration](#configuration)

---

## HTTP Handler

**Location**: [internal/handler/handler_new.go](../internal/handler/handler_new.go)

### Responsibilities

- Accept email messages via HTTP POST
- Validate message structure and size
- Authenticate requests (optional bearer token)
- Enqueue messages to SQLite queue
- Return immediate 202 response

### Design Decisions

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
[inbound]
tls_enabled = true
tls_cert_file = "/etc/letsencrypt/live/example.com/fullchain.pem"
tls_key_file = "/etc/letsencrypt/live/example.com/privkey.pem"
```

### Message Validation

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

## Queue System

**Location**: [internal/queue/queue.go](../internal/queue/queue.go)

### SQLite Schema

See [internal/queue/schema.go](../internal/queue/schema.go) for complete schema.

**Key tables**:
- `messages` - Main queue with status, retry times, delivery results
- `delivery_attempts` - Audit trail of all delivery attempts
- `callback_queue` - Webhook callback queue
- `mx_cache` - Cached MX records with TTL
- `idempotency_keys` - Prevents duplicate submissions
- `ip_reputation` - IP reputation tracking

### Design Decisions

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

**Why**: WAL supports 1 writer + N readers. We serialize writes with a mutex and allow concurrent reads by increasing the connection pool.

**Event Notification**
```go
notifyCh chan struct{}         // Notify workers of new messages
callbackNotifyCh chan struct{} // Notify callback handler
```

After any write (enqueue, retry schedule, callback), a non-blocking send notifies listeners.

**Why channels over polling**: Instant notification reduces delivery latency from "up to 30s" to "milliseconds".

### Message Lifecycle

1. **Enqueue**: Message inserted with status `queued`
2. **Dequeue**: Worker fetches batch, sets status to `sending`
3. **Delivery Attempt**: Engine attempts SMTP delivery
4. **Update Result**:
   - Success → `delivered`
   - Permanent failure → `hard_bounce`
   - Temporary failure → `queued` with next_retry_at
   - Expired → `expired`
5. **Callback**: Enqueue webhook notification
6. **Cleanup**: Old terminal messages removed

---

## Worker Pool

**Location**: [internal/worker/worker.go](../internal/worker/worker.go)

### Architecture

```go
type Worker struct {
  queue           *queue.Queue
  deliverer       *delivery.Deliverer
  retryScheduler  *delivery.RetryScheduler
  callbackHandler *callback.CallbackHandler
  config          *config.QueueConfig
  workers         []*workerInstance
  ctx             context.Context
  cancel          context.CancelFunc
}
```

### Design Decisions

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
  case <-notifyChan:                     // Instant notification
  case <-time.After(30*time.Second):     // Fallback poll
  case <-ctx.Done():                     // Graceful shutdown
}
```

**Why hybrid**: Events provide instant response, polling ensures messages aren't stuck if notification is missed (channel is non-blocking).

### Graceful Shutdown

```go
1. context.Cancel() signals all workers
2. In-flight deliveries check ctx.Done()
3. Wait for workers to finish current message
4. No new messages are fetched
```

**Why**: Prevents message loss during deploys. In-flight deliveries complete or are re-queued.

---

## Delivery Engine

**Location**: [internal/delivery/delivery.go](../internal/delivery/delivery.go)

### Core Flow

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

### Design Decisions

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
// Explicitly resolve, try all IPs
addrs := resolver.LookupHost(ctx, mxHost)
for _, ip := range addrs {
  conn := DialContext(ctx, "tcp6", ip+":25")
}
```

**Why**: Many large mail servers are multihomed for redundancy. Trying all IPs before moving to next MX priority improves deliverability.

**IP Limit Protection** (default: 5 IPs per MX)

```go
maxIPs := config.MaxIPsPerMX  // default: 5

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

---

## DNS Resolver

**Location**: [internal/delivery/dns_resolver.go](../internal/delivery/dns_resolver.go), [internal/delivery/mx_lookup.go](../internal/delivery/mx_lookup.go)

### Custom DNS Resolution

```go
type DNSResolver struct {
  resolvers []string  // Custom DNS servers (optional)
  timeout   time.Duration
  logger    *zap.Logger
}
```

### Design Decisions

**Custom DNS Servers** (optional)

```toml
[dns]
resolvers = ["8.8.8.8:53", "[2001:4860:4860::8888]:53"]
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

### MX Caching

```sql
CREATE TABLE mx_cache (
  domain TEXT PRIMARY KEY,
  mx_records TEXT NOT NULL,  -- JSON array
  cached_at TEXT NOT NULL,
  expires_at TEXT NOT NULL
);
```

**Cache Strategy**:
- Successful lookups: cached for `dns.cache_ttl_seconds` (default: 3600s / 1 hour)
- Failed lookups (NXDOMAIN): cached for `dns.cache_negative_ttl_seconds` (default: 60s)

**Why cache**:
- Reduces DNS query load (esp. for bulk sending)
- Improves delivery latency
- Respects DNS TTL while preventing excessive queries

---

## Callback System

**Location**: [internal/callback/callback.go](../internal/callback/callback.go)

### Callback Types

```go
type EventType string

const (
  EventDelivered  = "delivered"   // 2xx SMTP response
  EventFailed     = "failed"      // Temporary failure, will retry
  EventBounced    = "bounced"     // Permanent failure (5xx)
  EventExpired    = "expired"     // Exceeded max_message_age_hours
)
```

### Callback Payload

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

### Design Decisions

**Separate Callback Queue**

```sql
CREATE TABLE callback_queue (
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

---

## Configuration

**Location**: [internal/config/config.go](../internal/config/config.go)

### Configuration File Format (TOML)

See [config.toml.example](../config.toml.example) for complete configuration reference.

**Main sections**:
- `[inbound]` - HTTP API settings
- `[outbound]` - SMTP delivery settings
- `[queue]` - Queue and worker configuration
- `[dns]` - DNS resolution settings
- `[callbacks]` - Webhook configuration
- `[tls]` - TLS certificate management
- `[cluster]` - Gossip protocol (optional)
- `[metrics]` - Prometheus metrics
- `[health]` - Health check endpoint
- `[reputation]` - IP reputation tracking

### Design Decisions

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

## Related Documentation

- [Architecture Overview](architecture.md) - System design and philosophy
- [Security Features](security.md) - Authentication, TLS, and security
- [Error Handling](error-handling.md) - Retry logic and error classification
- [Monitoring](monitoring.md) - Metrics and observability
