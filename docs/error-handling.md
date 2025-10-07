# Error Handling & Retry Logic

This document describes how Fune classifies errors and schedules retries.

## Error Classification

**Location**: [internal/delivery/error_classifier.go](../internal/delivery/error_classifier.go)

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

### Classification Logic

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

## Retry Scheduling

**Location**: [internal/delivery/retry_scheduler.go](../internal/delivery/retry_scheduler.go)

```go
type RetryScheduler struct {
  initialDelay  time.Duration  // 5 minutes
  maxDelay      time.Duration  // 12 hours
  multiplier    float64        // 2.0
  greylistDelay time.Duration  // 2 minutes
}
```

### Retry Calculation

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

## Message Expiration

```go
max_message_age_hours = 48  // Default

if time.Now().After(msg.ExpiresAt) {
  status = StatusExpired
  // Send "expired" callback
  // Delete from queue
}
```

**Why 48 hours**: RFC 5321 recommends 4-5 days, but modern environments expect faster feedback. 48 hours balances retry attempts with timely bounce notifications.

## Anti-Spam Measures

### Destination Rate Limiting

```go
minInterval := 2 * time.Second  // Configurable

if lastAttempt[domain] + minInterval > now {
  return ErrorThrottled
}
lastAttempt[domain] = now
```

**Why**: Prevents rapid-fire connections to same domain. Spacing deliveries by 2+ seconds looks like legitimate mail server behavior, not bulk spam.

### IP Limit Per MX

```go
maxIPsPerMX := 5  // Configurable

// Try up to 5 IPs per MX, then move to next MX
```

**Why**: Malicious MX records could return hundreds of IPs to waste resources. Limiting prevents abuse.

### Connection Timeouts

```go
connectionTimeout := 30 * time.Second
smtpTimeout := 60 * time.Second
```

**Why**: Prevents tarpitting attacks where malicious servers keep connections open indefinitely.

## Configuration

```toml
[outbound]
# Retry settings
max_message_age_hours = 48
initial_retry_delay_seconds = 300      # 5 minutes
max_retry_delay_seconds = 43200        # 12 hours
backoff_multiplier = 2.0
greylist_retry_delay_seconds = 120     # 2 minutes

# Rate limiting
per_domain_interval_seconds = 2
throttle_retry_delay_seconds = 5

# Connection timeouts
connection_timeout_seconds = 30
smtp_timeout_seconds = 60
```

## Related Documentation

- [Architecture Overview](architecture.md)
- [Component Details](components.md)
- [Circuit Breaker](circuit-breaker.md)
- [IP Reputation](ip-reputation.md)
