# Circuit Breaker & Failover

This document describes the circuit breaker pattern and failover mechanisms in Fune.

## Circuit Breaker Pattern

**Purpose**: Automatically stop accepting HTTP requests when local delivery infrastructure fails, preventing queue buildup and providing fast-fail behavior.

### How It Works

```
1. Closed (normal): All requests accepted, deliveries attempted
2. Open (failing): Requests rejected with 503, deliveries paused
3. Half-Open (testing): Limited requests to test recovery
```

### Configuration

```toml
[outbound]
circuit_breaker_enabled = true            # Enable (recommended)
circuit_breaker_failure_threshold = 5     # Failures before opening
circuit_breaker_success_threshold = 2     # Successes to close
circuit_breaker_open_timeout_seconds = 60 # Wait before testing
```

### Trigger Conditions

**Opens on**:
- Network errors (connection refused, timeouts)
- DNS failures (NXDOMAIN, resolver unreachable)
- Local IP binding failures

**NOT triggered by**:
- Remote SMTP errors (4xx, 5xx codes)
- Greylisting (421)
- Rate limiting (our own throttling)

**Why**: Prevents cascading failures. If your network/DNS is down, rejecting requests immediately is better than queueing messages that can't be delivered.

### HTTP Response When Open

```json
{
  "error": "Service temporarily unavailable due to delivery failures",
  "retry_after": "60"
}
```

HTTP Status: `503 Service Unavailable`

## Source IP Failover

**Automatic rotation on local failures**:
```
1. Try delivery from source IP #1
2. If local network error → Try source IP #2
3. If local network error → Try source IP #3
4. If remote SMTP error → Move to next MX (don't rotate IPs)
```

### Configuration

```toml
[outbound]
source_ips = ["192.168.1.100", "192.168.1.101", "192.168.1.102"]
```

**Why**: If one local IP has network issues (routing, firewall, interface down), automatically fail over to another IP. Improves reliability without manual intervention.

### Example Scenario

```
Message to gmail.com
├─ MX: gmail-smtp-in.l.google.com
│  ├─ Try from 192.168.1.100 → Connection refused (local error)
│  ├─ Try from 192.168.1.101 → Success ✓
│  └─ (Circuit breaker: +1 failure, then +1 success)
```

## Integration with IP Reputation

Circuit breaker and IP reputation tracking work together:

- **Circuit Breaker**: Local infrastructure failures (affects all traffic)
- **IP Reputation**: Remote reputation issues (IP-specific)
- Different failure modes, different solutions

When circuit breaker opens, IP reputation tracking continues. When it closes, degraded IPs are still filtered from rotation.

## Monitoring

### Prometheus Metrics

```promql
# Current circuit breaker state (0=closed, 1=half-open, 2=open)
fune_circuit_breaker_state

# Circuit breaker state transitions
fune_circuit_breaker_transitions_total{from_state="closed",to_state="open"}
fune_circuit_breaker_transitions_total{from_state="open",to_state="half_open"}
fune_circuit_breaker_transitions_total{from_state="half_open",to_state="closed"}
```

### Recommended Alerts

```yaml
- alert: CircuitBreakerOpen
  expr: fune_circuit_breaker_state == 2
  for: 1m
  annotations:
    summary: "Fune circuit breaker is OPEN - delivery infrastructure failing"
```

## Related Documentation

- [Architecture Overview](architecture.md)
- [Error Handling](error-handling.md)
- [IP Reputation](ip-reputation.md)
- [Monitoring](monitoring.md)
