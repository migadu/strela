# IP Reputation Tracking

This document describes how Fune tracks and manages IP reputation to maintain deliverability.

## Overview

IP reputation tracking automatically detects when a source IP is being rejected by recipient mail servers and temporarily removes it from the rotation pool.

**Location**: [internal/delivery/ip_reputation.go](../internal/delivery/ip_reputation.go)

## How It Works

### Detection

After each delivery attempt, the system checks for reputation-related errors:

```go
// Keywords that indicate reputation issues
keywords := []string{
  "blocked", "blacklist", "spam", "reputation",
  "listed", "rbl", "dnsbl", "spamhaus",
}

// SMTP codes that indicate reputation issues
codes := []int{550, 554}

if containsKeyword(smtpResponse, keywords) && isReputationCode(smtpCode) {
  MarkIPDegraded(sourceIP, smtpCode, smtpResponse)
}
```

### Automatic IP Rotation Exclusion

Once an IP is marked as degraded:

1. **Removed from rotation**: IP is excluded from the source IP pool
2. **Existing deliveries continue**: In-flight messages from that IP complete
3. **New deliveries skip**: New messages use only healthy IPs
4. **Webhook notification**: Alert sent to configured webhook

### Recovery

```toml
[reputation]
degraded_ip_retry_hours = 48  # Default: 48 hours
```

After the retry period:
1. IP automatically returns to rotation pool
2. Next delivery tests if reputation has recovered
3. If still failing → marked degraded again
4. If successful → IP remains in pool

## Database Schema

```sql
CREATE TABLE ip_reputation (
  source_ip TEXT PRIMARY KEY,
  status TEXT NOT NULL,              -- 'degraded' or 'healthy'
  failure_count INTEGER DEFAULT 1,
  degraded_at TEXT,
  last_attempt_at TEXT,
  smtp_code INTEGER,
  smtp_response TEXT
);
```

## Configuration

```toml
[reputation]
enabled = true                      # Enable IP reputation tracking
degraded_ip_retry_hours = 48        # Hours before retry
webhook_url = "https://..."          # Alert webhook (optional)
```

## Webhook Notifications

When an IP is degraded or recovered, Fune sends a webhook:

```json
{
  "event_type": "ip_degraded",
  "source_ip": "192.168.1.100",
  "smtp_code": 550,
  "smtp_response": "5.7.1 IP address blocked by spamhaus",
  "timestamp": "2025-10-07T10:30:00Z",
  "degraded_at": "2025-10-07T10:30:00Z",
  "retry_after_hours": 48
}
```

## Monitoring

### CLI Commands

```bash
# View IP reputation status
./fune-admin reputation

# Example output:
SOURCE IP       STATUS          FAILURES  DEGRADED SINCE    CODE  RESPONSE
192.168.1.100  ⚠ DEGRADED      5         2 hours ago       550   5.7.1 blocked by spamhaus
192.168.1.101  ✓ healthy       0         -                 -     -
```

### Prometheus Metrics

```promql
# Number of degraded IPs
fune_ip_reputation_degraded{source_ip="192.168.1.100"} 1

# Reputation events
fune_ip_reputation_events_total{event_type="degraded",source_ip="192.168.1.100"}
fune_ip_reputation_events_total{event_type="recovered",source_ip="192.168.1.100"}
```

### Recommended Alerts

```yaml
- alert: IPReputationDegraded
  expr: fune_ip_reputation_degraded == 1
  for: 5m
  annotations:
    summary: "Source IP {{ $labels.source_ip }} has degraded reputation"
    description: "Check blacklist status and investigate cause"
```

## Best Practices

### Multiple Source IPs

Configure at least 3 source IPs for redundancy:

```toml
[outbound]
source_ips = [
  "192.168.1.100",
  "192.168.1.101",
  "192.168.1.102"
]
```

**Why**: If one IP gets blacklisted, service continues with remaining IPs.

### Regular Monitoring

Check reputation daily:
```bash
./fune-admin reputation
```

Monitor for degraded IPs and investigate root causes (compromised accounts, spam, misconfiguration).

### Blacklist Checking

When an IP is degraded, check major blacklists:

```bash
# Spamhaus
host 192.168.1.100.zen.spamhaus.org

# SpamCop
host 192.168.1.100.bl.spamcop.net

# Barracuda
host 192.168.1.100.b.barracudacentral.org
```

If blacklisted, submit delisting request:
- Spamhaus: https://www.spamhaus.org/lookup/
- SpamCop: https://www.spamcop.net/bl.shtml
- Barracuda: https://www.barracudacentral.org/rbl/removal-request

### Prevention

1. **Sender validation**: Require authentication on HTTP API
2. **Rate limiting**: Enable per-IP rate limiting
3. **Monitoring**: Alert on high bounce rates
4. **SPF/DKIM**: Configure proper email authentication

## Troubleshooting

### IP stays degraded after delisting

**Solution**: Manually reset in database:
```sql
UPDATE ip_reputation
SET status = 'healthy', degraded_at = NULL
WHERE source_ip = '192.168.1.100';
```

Then restart fune-server to reload IP pool.

### All IPs degraded

**Symptoms**: No deliveries succeeding, all IPs marked degraded

**Causes**:
1. Network misconfiguration affecting all IPs
2. Domain reputation issue (not IP-specific)
3. Incorrect SPF/DKIM configuration

**Solution**:
1. Check domain reputation at https://www.senderscore.org/
2. Verify SPF records: `dig TXT yourdomain.com`
3. Verify DKIM signing in logs
4. Review recent sending patterns for abuse

### False positives

**Symptom**: Legitimate IP marked degraded incorrectly

**Causes**:
- Temporary recipient server issue
- Overly sensitive keyword matching

**Solution**: Adjust keyword sensitivity or manually override in database.

## Related Documentation

- [Architecture Overview](architecture.md)
- [Circuit Breaker](circuit-breaker.md)
- [Error Handling](error-handling.md)
- [Monitoring](monitoring.md)
- [Disaster Recovery](disaster-recovery.md)
