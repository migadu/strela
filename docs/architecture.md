# Fune Architecture Overview

## Introduction

Fune is a high-performance, queue-based SMTP delivery service designed for reliable email delivery with proper retry logic, exponential backoff, and webhook callbacks. The architecture follows an event-driven design with clear separation of concerns.

## Component Flow

```
HTTP Request → Handler → Queue (SQLite) → Worker Pool → Delivery Engine → MX Servers
                              ↓                                ↓
                         Callback Queue ← Callback Handler ← Delivery Result
```

## Core Components

1. **HTTP Handler** - RESTful API for message submission
2. **Queue System** - SQLite-backed persistent queue with WAL mode
3. **Worker Pool** - Concurrent workers processing queued messages
4. **Delivery Engine** - Direct MX delivery with IPv6 support
5. **DNS Resolver** - Custom DNS resolution with caching
6. **Callback System** - Webhook notifications for delivery events

## Design Philosophy

### 1. Asynchronous by Design

The service immediately returns `202 Accepted` after validating and enqueuing messages. Actual delivery happens asynchronously in background workers. This prevents HTTP timeout issues and allows for proper retry handling.

**Why**: SMTP delivery can take seconds or fail requiring retries over hours/days. Synchronous delivery would tie up HTTP connections and prevent proper backoff.

### 2. Event-Driven Architecture

Instead of polling, the system uses Go channels for instant notifications when new messages or callbacks are queued. A fallback polling mechanism (30s default) ensures reliability.

**Why**: Reduces latency (messages start delivery immediately) and CPU usage (no constant polling).

### 3. Persistent Queue with SQLite

SQLite in WAL (Write-Ahead Logging) mode provides:
- Single writer + multiple concurrent readers
- ACID transactions without external dependencies
- Simple deployment (single file database)

**Why**: Redis/RabbitMQ add operational complexity. SQLite provides persistence, concurrency, and reliability in a zero-dependency package.

### 4. Context Propagation

All operations accept `context.Context` for graceful cancellation:
- HTTP server shutdown cancels in-flight deliveries
- DNS queries respect context timeouts
- SMTP connections can be interrupted

**Why**: Prevents hanging goroutines during shutdown and enables proper timeout handling throughout the stack.

### 5. IPv6-First Delivery

The delivery engine attempts IPv6 before IPv4, following modern internet standards.

**Why**: IPv6 is increasingly preferred by major mail providers and can offer better deliverability.

## Message States

```go
const (
  StatusQueued      = "queued"      // Ready for delivery
  StatusSending     = "sending"     // Worker actively delivering
  StatusDelivered   = "delivered"   // Successfully delivered
  StatusHardBounce  = "hard_bounce" // Permanent failure (5xx)
  StatusTempExpired = "temp_expired"// Temporary failure expired
  StatusExpired     = "expired"     // Exceeded max_message_age_hours
)
```

**State Transitions**:
```
queued → sending → delivered
              → hard_bounce
              → temp_expired (retry)
              → expired
```

## Performance Characteristics

### SQLite WAL Mode
**Benefit**: 10x more concurrent read operations (1 writer + 10 readers vs 1 connection).

### Event-Driven Workers
**Benefit**: Messages deliver in milliseconds, not "up to 30 seconds". Reduces average latency 100x.

### MX Caching
**Benefit**: Eliminates DNS query for repeated deliveries to same domain. Reduces delivery latency by ~50-200ms per message.

### Connection Pool
```go
db.SetMaxOpenConns(10)   // Multiple concurrent readers
db.SetMaxIdleConns(5)    // Keep connections warm
```

**Benefit**: Workers can query queue concurrently without lock contention on reads.

## Technology Stack

- **Language**: Go 1.21+
- **Database**: SQLite 3 with WAL mode
- **Logging**: go.uber.org/zap (structured logging)
- **Metrics**: Prometheus client
- **Config**: TOML format
- **Clustering**: HashiCorp memberlist (optional)

## Scalability Considerations

### Current Architecture
- Single instance (SQLite limits to one writer)
- Handles thousands of messages per hour per instance
- Vertical scaling via increased worker count

### Future Scaling Options
- Partition by domain/hash for multiple instances
- Each instance has independent SQLite database
- Load balancer distributes by hash(recipient_domain)

## Design Trade-offs

| Decision | Trade-off | Rationale |
|----------|-----------|-----------|
| SQLite over Redis | No horizontal scaling | Simpler deployment, zero dependencies |
| Event-driven workers | Slightly more complex code | 100x better latency worth the complexity |
| IPv6-first | May fail on IPv4-only networks | Modern best practice, fallback available |
| 48-hour max age | Faster failure feedback | Balance between retry attempts and timely bounces |
| Destination throttling | Lower throughput to same domain | Better reputation, appears legitimate |

## Related Documentation

- [Component Details](components.md) - Deep dive into each component
- [Security Features](security.md) - Authentication, TLS, and security measures
- [Deployment Guide](deployment.md) - Production deployment considerations
- [Monitoring Guide](monitoring.md) - Metrics and observability
