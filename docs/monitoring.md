# Monitoring & Observability

This document describes how to monitor Fune in production using metrics, logs, and health checks.

## Prometheus Metrics

**Endpoint**: `http://localhost:9090/metrics` (configurable)

**Location**: [internal/metrics/metrics.go](../internal/metrics/metrics.go)

### Configuration

```toml
[metrics]
enabled = true
listen = ":9090"
path = "/metrics"
```

### Available Metrics

#### Queue Metrics

```promql
# Queue depth by status
fune_queue_depth{status="queued"}
fune_queue_depth{status="sending"}
fune_queue_depth{status="delivered"}
fune_queue_depth{status="hard_bounce"}
```

**Alert Examples**:
```yaml
- alert: QueueBacklog
  expr: fune_queue_depth{status="queued"} > 10000
  for: 5m

- alert: HighBounceRate
  expr: rate(fune_queue_depth{status="hard_bounce"}[5m]) > 10
  for: 5m
```

#### Delivery Metrics

```promql
# Delivery attempts by outcome
fune_delivery_attempts_total{outcome="success"}
fune_delivery_attempts_total{outcome="temporary_error"}
fune_delivery_attempts_total{outcome="permanent_error"}
fune_delivery_attempts_total{outcome="network_error"}

# Delivery duration
fune_delivery_duration_seconds_bucket{outcome="success"}
fune_delivery_duration_seconds_sum{outcome="success"}
fune_delivery_duration_seconds_count{outcome="success"}
```

**Query Examples**:
```promql
# Success rate
rate(fune_delivery_attempts_total{outcome="success"}[5m])
/
rate(fune_delivery_attempts_total[5m])

# p95 delivery latency
histogram_quantile(0.95, rate(fune_delivery_duration_seconds_bucket[5m]))

# Messages per minute
rate(fune_delivery_attempts_total[1m]) * 60
```

#### Callback Metrics

```promql
# Callback attempts
fune_callback_attempts_total{outcome="success",event_type="delivered"}
fune_callback_attempts_total{outcome="failure",event_type="bounced"}

# Callback duration
fune_callback_duration_seconds_bucket
```

#### HTTP Metrics

```promql
# HTTP requests
fune_http_requests_total{method="POST",path="/v1/messages",status="202"}
fune_http_requests_total{method="POST",path="/v1/messages",status="429"}

# HTTP latency
fune_http_request_duration_seconds_bucket{method="POST",path="/v1/messages"}
```

**Alert Example**:
```yaml
- alert: HighHTTPErrorRate
  expr: rate(fune_http_requests_total{status=~"5.."}[5m]) > 10
  for: 5m
```

#### Circuit Breaker Metrics

```promql
# Circuit breaker state (0=closed, 1=half-open, 2=open)
fune_circuit_breaker_state

# State transitions
fune_circuit_breaker_transitions_total{from_state="closed",to_state="open"}
```

#### IP Reputation Metrics

```promql
# Degraded IPs
fune_ip_reputation_degraded{source_ip="192.168.1.100"}

# Reputation events
fune_ip_reputation_events_total{event_type="degraded",source_ip="192.168.1.100"}
```

#### Database Metrics

```promql
# Database size
fune_database_size_bytes

# WAL size
fune_database_wal_size_bytes

# Active connections
fune_database_connections

# Query duration
fune_database_query_duration_seconds_bucket{operation="enqueue"}
```

**Alert Example**:
```yaml
- alert: DatabaseSizeGrowing
  expr: fune_database_size_bytes > 10e9  # 10GB
  for: 1h
  annotations:
    summary: "Database is larger than 10GB, consider archiving old messages"
```

## Structured Logging

Fune uses [Zap](https://github.com/uber-go/zap) for structured logging.

### Log Levels

```go
DEBUG - DNS queries, MX lookups, detailed flow
INFO  - Message enqueued, delivery success, worker start/stop
WARN  - Retry scheduled, circuit breaker state changes
ERROR - Delivery failures, database errors, panics
```

### Configuration

```toml
[server]
log_level = "info"  # debug, info, warn, error
log_format = "json" # json or console
```

### Example Log Entries

**Message enqueued**:
```json
{
  "level": "info",
  "ts": "2025-10-07T10:30:00.000Z",
  "msg": "message enqueued",
  "message_id": "msg_abc123",
  "to": "user@example.com",
  "expires_at": "2025-10-09T10:30:00.000Z"
}
```

**Delivery success**:
```json
{
  "level": "info",
  "ts": "2025-10-07T10:30:15.000Z",
  "msg": "delivery successful",
  "message_id": "msg_abc123",
  "to": "user@example.com",
  "mx_host": "mx1.example.com",
  "source_ip": "192.168.1.100",
  "attempts": 1,
  "smtp_code": 250
}
```

**Delivery failure**:
```json
{
  "level": "warn",
  "ts": "2025-10-07T10:30:15.000Z",
  "msg": "delivery failed, scheduling retry",
  "message_id": "msg_abc123",
  "to": "user@example.com",
  "attempts": 2,
  "error": "450 4.2.1 Mailbox temporarily unavailable",
  "next_retry": "2025-10-07T10:35:15.000Z"
}
```

### Log Aggregation

For production, send logs to a centralized system:

**Filebeat → Elasticsearch**:
```yaml
filebeat.inputs:
  - type: log
    enabled: true
    paths:
      - /var/log/fune/*.log
    json.keys_under_root: true
```

**Promtail → Loki**:
```yaml
scrape_configs:
  - job_name: fune
    static_configs:
      - targets:
          - localhost
        labels:
          job: fune
          __path__: /var/log/fune/*.log
```

## Health Checks

### HTTP Health Endpoint

**Endpoint**: `GET /health`

**Response** (healthy):
```json
{
  "status": "healthy",
  "timestamp": "2025-10-07T10:30:00Z",
  "uptime": "5h30m",
  "queue": {
    "pending": 150
  },
  "database": {
    "size_mb": 45.2,
    "wal_size_mb": 2.1,
    "connections": 5,
    "fragment_percent": 8.5,
    "cache_hit_percent": 92.3,
    "queued_messages": 150,
    "sending_messages": 10
  },
  "circuit_breaker": {
    "state": "closed",
    "failures": 0,
    "successes": 150
  },
  "system": {
    "go_version": "go1.21.0",
    "goroutines": 25,
    "memory_mb": 120,
    "memory_alloc_mb": 45
  }
}
```

**Response** (degraded):
```json
{
  "status": "degraded",
  "timestamp": "2025-10-07T10:30:00Z",
  "queue": {"pending": 15000},
  "database": {
    "size_mb": 12500.0,
    "fragment_percent": 45.0
  },
  "circuit_breaker": {"state": "open"}
}
```

HTTP Status: `503 Service Unavailable` when unhealthy or degraded

### CLI Health Check

```bash
./fune-admin health

# Output:
Health Status
==========================================================================================
Status:    HEALTHY
Timestamp: 2025-10-07T10:30:00Z
Uptime:    5h30m

Queue:
  Pending:   150

Database:
  Size:           45.20 MB
  WAL Size:       2.10 MB
  Connections:    5
  Fragment:       8.5%
  Cache Hit Rate: 92.3%
  Queued:         150 messages
  Sending:        10 messages

Circuit Breaker:
  State:          CLOSED
  Failures:       0
  Successes:      150

System:
  Go Version:     go1.21.0
  Goroutines:     25
  Memory (Total): 120 MB
  Memory (Alloc): 45 MB
```

### Liveness & Readiness Probes

For Kubernetes/container orchestration:

**Liveness** (is process alive?):
```yaml
livenessProbe:
  httpGet:
    path: /health
    port: 8080
  initialDelaySeconds: 30
  periodSeconds: 10
```

**Readiness** (can accept traffic?):
```yaml
readinessProbe:
  httpGet:
    path: /health
    port: 8080
  initialDelaySeconds: 5
  periodSeconds: 5
```

## Admin CLI Tools

### Queue Statistics

```bash
./fune-admin queue

# Output:
Queue Statistics
==========================================
Total:        1,523 messages
Queued:       150 messages
Sending:      10 messages
Delivered:    1,200 messages
Hard Bounce:  150 messages
Expired:      13 messages

Oldest Queued: 2 hours ago
```

### Throughput Statistics

```bash
./fune-admin throughput

# Output:
Delivery Throughput
==========================================
Last 1 Hour:    450 deliveries
Last 6 Hours:   2,100 deliveries
Last 24 Hours:  8,500 deliveries
Last 7 Days:    52,000 deliveries

Success Rate:   92.5%
Total Attempts: 56,000
```

### Recent Failures

```bash
./fune-admin failures

# Output (last 20):
Recent Delivery Failures
======================================================================================
MESSAGE ID          TIME        MX HOST           CODE  RESPONSE
msg_abc123...       5m ago      mx1.example.com   550   5.7.1 Sender rejected
msg_def456...       12m ago     mx2.example.com   451   4.3.0 Temporary failure
```

### Database Statistics

```bash
./fune-admin database

# Output:
Database Statistics
==========================================================================================

Storage:
  Database Size:     45.20 MB
  WAL Size:          2.10 MB
  Total Size:        47.30 MB
  Page Size:         4096 bytes
  Page Count:        11595

Performance:
  Active Connections: 5
  Cache Hit Rate:     92.3%
  Fragmentation:      8.5%
  WAL Checkpoints:    142

Queue Depth:
  Queued Messages:    150
  Sending Messages:   10
  Total Active:       160

Recommendations:
  ✓ Database health looks good
```

## Recommended Dashboard

Grafana dashboard example:

**Panels**:
1. Queue depth over time (line graph)
2. Delivery success rate (gauge)
3. Delivery attempts by outcome (stacked area)
4. HTTP requests per minute (line graph)
5. Circuit breaker state (stat panel)
6. Degraded IPs (table)
7. p95 delivery latency (line graph)
8. Database size trend (line graph)

**PromQL Queries**:
```promql
# Queue backlog
fune_queue_depth{status="queued"}

# Success rate
sum(rate(fune_delivery_attempts_total{outcome="success"}[5m]))
/
sum(rate(fune_delivery_attempts_total[5m]))

# Requests per minute
rate(fune_http_requests_total[1m]) * 60

# p95 latency
histogram_quantile(0.95, rate(fune_delivery_duration_seconds_bucket[5m]))
```

## Related Documentation

- [Architecture Overview](architecture.md)
- [Deployment Guide](deployment.md)
- [Disaster Recovery](disaster-recovery.md)
- [Circuit Breaker](circuit-breaker.md)
