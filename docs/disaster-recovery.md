# Fune Disaster Recovery Procedures

This document provides comprehensive disaster recovery procedures, incident response runbooks, and emergency protocols for Fune SMTP delivery service.

## Table of Contents

- [Overview](#overview)
- [Emergency Contacts](#emergency-contacts)
- [Service Architecture](#service-architecture)
- [Disaster Scenarios](#disaster-scenarios)
  - [1. Database Corruption](#1-database-corruption)
  - [2. Queue Overflow / Disk Full](#2-queue-overflow--disk-full)
  - [3. Service Crash / OOM Kill](#3-service-crash--oom-kill)
  - [4. Network Partition / DNS Failure](#4-network-partition--dns-failure)
  - [5. IP Reputation Crisis](#5-ip-reputation-crisis)
  - [6. Circuit Breaker Stuck Open](#6-circuit-breaker-stuck-open)
  - [7. TLS Certificate Expiration](#7-tls-certificate-expiration)
  - [8. Complete Server Loss](#8-complete-server-loss)
  - [9. Message Delivery Stall](#9-message-delivery-stall)
  - [10. Callback Webhook Failure](#10-callback-webhook-failure)
- [Incident Response Procedures](#incident-response-procedures)
- [Communication Templates](#communication-templates)
- [Post-Incident Review](#post-incident-review)

---

## Overview

**Purpose**: This document provides step-by-step procedures for recovering from disasters, outages, and critical incidents affecting the Fune SMTP delivery service.

**Scope**: Covers operational disasters, data loss scenarios, infrastructure failures, and service degradation.

**Recovery Time Objectives (RTO)**:
- P0 (Critical): 15 minutes
- P1 (High): 1 hour
- P2 (Medium): 4 hours
- P3 (Low): 24 hours

**Recovery Point Objectives (RPO)**:
- Database loss: 6 hours (backup frequency)
- Configuration loss: Immediate (version controlled)

---

## Emergency Contacts

```
Primary On-Call:    [PHONE] [EMAIL]
Secondary On-Call:  [PHONE] [EMAIL]
Team Lead:          [PHONE] [EMAIL]
Infrastructure:     [PHONE] [EMAIL]
```

**External Contacts**:
- Cloud Provider Support: [PHONE/TICKET]
- DNS Provider: [SUPPORT URL]
- Monitoring Service: [CONTACT]

**Escalation Path**:
1. Primary On-Call (0-15 minutes)
2. Secondary On-Call (15-30 minutes)
3. Team Lead (30-60 minutes)
4. Engineering Manager (60+ minutes)

---

## Service Architecture

Understanding the architecture is critical for effective disaster recovery.

```
┌─────────────┐
│   Clients   │
└──────┬──────┘
       │ HTTPS
┌──────▼───────────┐
│  Fune Server(s)  │
│  - HTTP Handler  │──────▶ TLS via Let's Encrypt
│  - TLS Manager   │        (auto-renewal)
│  - Queue         │
│  - Workers       │
│  - Deliverer     │
└──────┬───────────┘
       │
┌──────▼───────────┐      ┌──────────────┐
│   SQLite DB      │      │   DNS        │
│   - queue.db     │      │   Resolvers  │
│   - queue.db-wal │      └──────────────┘
│   - queue.db-shm │
└──────────────────┘      ┌──────────────┐
                          │   MX Servers │
       │                  └──────────────┘
       ▼
┌──────────────────┐
│ Webhook Endpoint │
└──────────────────┘
```

**Critical Dependencies**:
- SQLite database (single point of failure)
- DNS resolution (external dependency)
- Destination MX servers (external)
- Webhook endpoint (optional)

---

## Disaster Scenarios

### 1. Database Corruption

**Severity**: P0 (Critical)
**Symptoms**:
- Service fails to start with SQLite errors
- `PRAGMA integrity_check` fails
- Logs show "database disk image is malformed"
- Workers crash when querying database

**Immediate Actions** (5 minutes):

```bash
# 1. Stop the service immediately
systemctl stop fune-server

# 2. Check database integrity
sqlite3 queue.db "PRAGMA integrity_check;"

# 3. If corruption confirmed, attempt recovery
mkdir -p /var/recovery
sqlite3 queue.db ".recover" | sqlite3 /var/recovery/recovered.db

# 4. Check recovered database
sqlite3 /var/recovery/recovered.db "PRAGMA integrity_check;"

# 5. Compare message counts
echo "Original DB:"
sqlite3 queue.db "SELECT COUNT(*) FROM messages;" 2>/dev/null || echo "FAILED"
echo "Recovered DB:"
sqlite3 /var/recovery/recovered.db "SELECT COUNT(*) FROM messages;"
```

**Recovery Options**:

**Option A: Recovery Successful**
```bash
# Backup corrupted database for forensics
mv queue.db queue.db.corrupted.$(date +%Y%m%d_%H%M%S)
mv queue.db-wal queue.db-wal.corrupted 2>/dev/null
mv queue.db-shm queue.db-shm.corrupted 2>/dev/null

# Use recovered database
mv /var/recovery/recovered.db queue.db

# Set correct permissions
chown fune:fune queue.db
chmod 640 queue.db

# Start service
systemctl start fune-server

# Monitor logs
tail -f /var/log/fune/fune.log
```

**Option B: Recovery Failed - Restore from Backup**
```bash
# Find latest backup
LATEST_BACKUP=$(find /var/backups/fune -name "queue_backup_*.db.gz" | sort | tail -1)
echo "Restoring from: $LATEST_BACKUP"

# Backup corrupted database
mv queue.db queue.db.corrupted.$(date +%Y%m%d_%H%M%S)

# Restore
gunzip -c "$LATEST_BACKUP" > queue.db
chown fune:fune queue.db
chmod 640 queue.db

# Start service
systemctl start fune-server
```

**Post-Recovery**:
```bash
# Verify service health
./fune-admin queue
./fune-admin health

# Check for lost messages
./fune-admin queue -db queue.db.corrupted.* 2>/dev/null | grep "Queued:"
./fune-admin queue | grep "Queued:"
# Document any message loss

# Monitor disk health
smartctl -a /dev/sda | grep -i error
df -h
```

**Root Cause Analysis**:
- Check disk errors: `dmesg | grep -i error`
- Review filesystem: `fsck` or `xfs_repair`
- Check system resources: OOM events, disk full
- Review backup logs for previous corruption signs

**Prevention**:
- Enable filesystem journaling (ext4, XFS)
- Monitor disk health with SMART
- Ensure adequate disk space (>20% free)
- Regular backup integrity checks

---

### 2. Queue Overflow / Disk Full

**Severity**: P1 (High)
**Symptoms**:
- HTTP API returns "Failed to enqueue message"
- Disk usage at 100%
- Service becomes unresponsive
- `SQLITE_FULL` errors in logs

**Immediate Actions** (10 minutes):

```bash
# 1. Check disk space
df -h
du -sh /var/lib/fune/*

# 2. Check queue size
./fune-admin queue
sqlite3 queue.db "SELECT COUNT(*), SUM(length(raw_message)) FROM messages;"

# 3. Emergency: Stop accepting new messages
# Option A: Stop service temporarily
systemctl stop fune-server

# Option B: Block HTTP port
iptables -A INPUT -p tcp --dport 8080 -j DROP
```

**Recovery Actions**:

**Step 1: Free up disk space**
```bash
# Remove old delivery attempt logs (if table is large)
sqlite3 queue.db "DELETE FROM delivery_attempts WHERE attempted_at < datetime('now', '-30 days');"

# Remove old completed callbacks
sqlite3 queue.db "DELETE FROM callback_queue WHERE completed_at < datetime('now', '-7 days');"

# Vacuum database to reclaim space
sqlite3 queue.db "VACUUM;"

# Check freed space
df -h
```

**Step 2: Archive delivered messages**
```bash
# Export delivered messages
sqlite3 queue.db <<EOF
.mode csv
.output /var/backups/fune/delivered_$(date +%Y%m%d).csv
SELECT * FROM messages WHERE status = 'delivered' AND created_at < datetime('now', '-7 days');
.quit
EOF

# Delete archived messages
sqlite3 queue.db "DELETE FROM messages WHERE status = 'delivered' AND created_at < datetime('now', '-7 days');"

# Vacuum
sqlite3 queue.db "VACUUM;"
```

**Step 3: Increase disk space** (if needed)
```bash
# Extend volume (cloud provider specific)
# AWS: Modify EBS volume size
# GCP: Resize persistent disk

# Resize filesystem
resize2fs /dev/xvdf  # ext4
xfs_growfs /var/lib/fune  # XFS
```

**Step 4: Resume service**
```bash
# Re-enable HTTP port
iptables -D INPUT -p tcp --dport 8080 -j DROP

# Start service
systemctl start fune-server

# Monitor
./fune-admin throughput
tail -f /var/log/fune/fune.log
```

**Prevention**:
```bash
# Set up disk space monitoring
cat > /etc/cron.hourly/fune-disk-check <<'EOF'
#!/bin/bash
DISK_USAGE=$(df -h /var/lib/fune | tail -1 | awk '{print $5}' | sed 's/%//')
if [ $DISK_USAGE -gt 80 ]; then
    echo "WARNING: Disk usage at ${DISK_USAGE}%"
    # Send alert
fi
EOF
chmod +x /etc/cron.hourly/fune-disk-check

# Implement automatic cleanup
cat >> config.toml <<EOF
[queue]
cleanup_interval_seconds = 3600  # Clean up old records hourly
max_delivered_age_days = 7       # Keep delivered messages for 7 days
max_failed_age_days = 30         # Keep failed messages for 30 days
EOF
```

---

### 3. Service Crash / OOM Kill

**Severity**: P1 (High)
**Symptoms**:
- Service stops unexpectedly
- `dmesg` shows "Out of memory: Kill process"
- HTTP endpoints unreachable
- No response to health checks

**Immediate Actions** (5 minutes):

```bash
# 1. Check if service is running
systemctl status fune-server

# 2. Check for OOM kills
dmesg | grep -i "out of memory"
dmesg | grep -i "killed process"

# 3. Check recent crashes
journalctl -u fune-server -n 100 --no-pager

# 4. Check system resources
free -h
top -n 1 -b | head -20
```

**Recovery Actions**:

**For OOM Kill**:
```bash
# 1. Identify memory issue
journalctl -u fune-server | grep -i "memory"

# 2. Check configuration
cat config.toml | grep worker_count
cat config.toml | grep batch_size

# 3. Reduce worker count temporarily
sed -i 's/worker_count = .*/worker_count = 5/' config.toml

# 4. Restart service
systemctl start fune-server

# 5. Monitor memory usage
watch -n 5 'ps aux | grep fune-server | grep -v grep'
```

**For Panic/Crash**:
```bash
# 1. Check for core dumps
ls -lh /var/crash/
coredumpctl list

# 2. Review crash logs
journalctl -u fune-server --since "10 minutes ago" | grep -i "panic\|fatal\|error"

# 3. Check for known issues
./fune-server --version
# Check release notes for bug fixes

# 4. Restart with verbose logging
export LOG_LEVEL=debug
systemctl restart fune-server

# 5. Monitor for recurrence
tail -f /var/log/fune/fune.log | grep -i "error\|panic\|fatal"
```

**Prevent Future OOM**:
```toml
# config.toml
[queue]
worker_count = 10        # Reduce if memory constrained
batch_size = 5           # Smaller batches = less memory

[inbound]
max_body_size_bytes = 10485760  # 10MB limit

# Add memory limits in systemd
# /etc/systemd/system/fune-server.service
[Service]
MemoryMax=2G
MemoryHigh=1.5G
```

**Configure Automatic Restart**:
```ini
# /etc/systemd/system/fune-server.service
[Service]
Restart=always
RestartSec=10
StartLimitBurst=5
StartLimitIntervalSec=300
```

```bash
systemctl daemon-reload
systemctl restart fune-server
```

---

### 4. Network Partition / DNS Failure

**Severity**: P1 (High)
**Symptoms**:
- All deliveries failing with "DNS lookup failed"
- MX lookups timing out
- Workers stuck in retry loops
- Circuit breaker opens

**Immediate Actions** (5 minutes):

```bash
# 1. Test DNS resolution
dig example.com MX +short
nslookup -type=MX gmail.com 8.8.8.8

# 2. Check configured DNS servers
cat config.toml | grep -A 5 "\[dns\]"

# 3. Test system DNS
getent hosts google.com

# 4. Check network connectivity
ping -c 3 8.8.8.8
traceroute 8.8.8.8
```

**Recovery Actions**:

**If DNS servers unreachable**:
```bash
# 1. Update to public DNS temporarily
cat > config.toml.patch <<EOF
[dns]
resolvers = ["8.8.8.8:53", "1.1.1.1:53", "8.8.4.4:53"]
timeout_seconds = 10
EOF

# 2. Apply configuration
# Merge patch into config.toml

# 3. Reload configuration (hot reload)
kill -SIGHUP $(cat fune.pid)

# 4. Verify DNS resolution
./fune-admin health
tail -f /var/log/fune/fune.log | grep -i "dns\|mx"
```

**If network partition**:
```bash
# 1. Check network routes
ip route show
netstat -rn

# 2. Check firewall rules
iptables -L -n -v
ufw status verbose

# 3. Test SMTP port connectivity
nc -zv gmail-smtp-in.l.google.com 25
telnet gmail-smtp-in.l.google.com 25

# 4. Check for network interface issues
ip addr show
ethtool eth0  # Check link status
```

**If circuit breaker stuck open**:
```bash
# 1. Check circuit breaker status
./fune-admin health

# 2. Review recent failures
./fune-admin failures | head -20

# 3. If DNS is working now, wait for circuit breaker recovery
# Default: 60 seconds timeout

# 4. Or restart service to reset circuit breaker
systemctl restart fune-server
```

**Prevention**:
```toml
# config.toml - Use multiple DNS servers
[dns]
resolvers = [
    "8.8.8.8:53",           # Google Primary
    "8.8.4.4:53",           # Google Secondary
    "1.1.1.1:53",           # Cloudflare Primary
    "1.0.0.1:53",           # Cloudflare Secondary
]
timeout_seconds = 5
cache_ttl_seconds = 3600
cache_negative_ttl_seconds = 60
```

---

### 5. IP Reputation Crisis

**Severity**: P0 (Critical)
**Symptoms**:
- High bounce rate (>20%)
- Multiple IPs marked as degraded
- SMTP errors: "550 Sender rejected", "554 Blocked"
- Delivery rate drops significantly

**Immediate Actions** (15 minutes):

```bash
# 1. Check IP reputation status
./fune-admin reputation

# 2. Review recent failures by SMTP code
./fune-admin failures | grep -E "550|554" | head -20

# 3. Check delivery rate
./fune-admin throughput

# 4. Identify affected IPs
sqlite3 queue.db "SELECT source_ip, COUNT(*) FROM delivery_attempts WHERE smtp_code IN (550, 554) GROUP BY source_ip ORDER BY COUNT(*) DESC;"
```

**Emergency Mitigation**:

**Option 1: Remove degraded IPs from rotation**
```bash
# 1. Identify degraded IPs
DEGRADED_IPS=$(./fune-admin reputation | grep "degraded" | awk '{print $1}')

# 2. Update configuration to exclude them
# Edit config.toml and remove degraded IPs from source_ips

# 3. Hot reload configuration
kill -SIGHUP $(cat fune.pid)

# 4. Verify IPs in use
./fune-admin config | grep source_ips
```

**Option 2: Route through backup IPs**
```toml
# config.toml
[outbound]
source_ips = [
    "192.168.1.50",  # Backup IP 1
    "192.168.1.51",  # Backup IP 2
]
source_ip_selection = "round-robin"
```

```bash
kill -SIGHUP $(cat fune.pid)
```

**Long-term Recovery**:

1. **Request delisting** (if blacklisted):
```bash
# Check blacklist status
for ip in 192.168.1.100 192.168.1.101; do
    echo "Checking $ip..."
    host $ip.zen.spamhaus.org
    host $ip.bl.spamcop.net
    host $ip.b.barracudacentral.org
done

# Submit delisting requests:
# - Spamhaus: https://www.spamhaus.org/lookup/
# - SpamCop: https://www.spamcop.net/bl.shtml
# - Barracuda: https://www.barracudacentral.org/rbl/removal-request
```

2. **Investigate root cause**:
```bash
# Check for spam sources
sqlite3 queue.db "SELECT from_addr, COUNT(*) as cnt FROM messages WHERE created_at > datetime('now', '-1 day') GROUP BY from_addr ORDER BY cnt DESC LIMIT 20;"

# Review message content
sqlite3 queue.db "SELECT subject, from_addr, to_addr FROM messages WHERE status = 'hard_bounce' LIMIT 10;"
```

3. **Implement sender validation**:
```toml
# config.toml
[inbound]
auth_token = "strong-random-token-here"  # Enforce authentication
rate_limit_enabled = true
rate_limit_requests_per_ip = 100
```

**Prevention**:
- Monitor IP reputation daily
- Implement authentication on HTTP API
- Rate limit per sender
- Monitor bounce rates with alerts
- Maintain warm backup IPs
- Rotate IPs periodically

---

### 6. Circuit Breaker Stuck Open

**Severity**: P1 (High)
**Symptoms**:
- HTTP API returns "503 Service Unavailable"
- "Circuit breaker open, rejecting request" in logs
- No deliveries being attempted
- Queue continues to grow

**Immediate Actions** (5 minutes):

```bash
# 1. Check circuit breaker status
./fune-admin health | grep -i circuit

# 2. Review recent failures
./fune-admin failures | head -20

# 3. Check error types
./fune-admin failures | awk '{print $NF}' | sort | uniq -c | sort -rn
```

**Recovery Actions**:

**If legitimate failures** (e.g., DNS issues resolved):
```bash
# Option 1: Wait for automatic recovery (60 seconds default)
watch -n 10 './fune-admin health'

# Option 2: Restart service to reset circuit breaker
systemctl restart fune-server

# Option 3: Temporarily disable circuit breaker
# Edit config.toml:
# [outbound]
# circuit_breaker_enabled = false

kill -SIGHUP $(cat fune.pid)
```

**If circuit breaker too sensitive**:
```toml
# config.toml
[outbound]
circuit_breaker_enabled = true
circuit_breaker_failure_threshold = 10  # Increase from 5
circuit_breaker_success_threshold = 3   # Increase from 2
circuit_breaker_open_timeout_seconds = 120  # Increase from 60
```

```bash
kill -SIGHUP $(cat fune.pid)
```

**Monitoring**:
```bash
# Watch circuit breaker state changes
tail -f /var/log/fune/fune.log | grep -i "circuit breaker"

# Monitor recovery attempts
./fune-admin throughput
```

---

### 7. TLS Certificate Expiration

**Severity**: P1 (High)
**Symptoms**:
- HTTPS endpoint inaccessible
- "TLS handshake failed" errors
- Certificate warnings in browsers
- Webhook callbacks failing

**Immediate Actions** (10 minutes):

```bash
# 1. Check certificate expiration
openssl x509 -in /path/to/cert.pem -noout -dates

# 2. Check current TLS configuration
cat config.toml | grep -A 10 "\[tls\]"

# 3. Test TLS endpoint
openssl s_client -connect localhost:8080 -servername mail.example.com < /dev/null
```

**Recovery Actions**:

**For Let's Encrypt auto-renewal failure**:
```bash
# 1. Check Let's Encrypt status
certbot certificates

# 2. Force renewal
certbot renew --force-renewal

# 3. Reload Fune to pick up new cert
# (Hot reload monitors cert files)
kill -SIGHUP $(cat fune.pid)

# 4. Verify new certificate
openssl x509 -in /etc/letsencrypt/live/mail.example.com/fullchain.pem -noout -dates
```

**For manual certificate**:
```bash
# 1. Generate new certificate (example: self-signed for emergency)
openssl req -x509 -newkey rsa:4096 -nodes \
    -keyout /tmp/key.pem \
    -out /tmp/cert.pem \
    -days 365 \
    -subj "/CN=mail.example.com"

# 2. Update configuration
# Edit config.toml:
# [tls]
# cert_file = "/tmp/cert.pem"
# key_file = "/tmp/key.pem"

# 3. Hot reload
kill -SIGHUP $(cat fune.pid)

# 4. Verify
curl -v https://localhost:8080/health
```

**Emergency: Disable TLS temporarily**:
```toml
# config.toml
[tls]
enabled = false

[inbound]
listen = ":8080"  # Use HTTP temporarily
```

```bash
kill -SIGHUP $(cat fune.pid)
```

**Prevention**:
```bash
# Set up certificate expiration monitoring
cat > /etc/cron.daily/check-tls-cert <<'EOF'
#!/bin/bash
CERT="/etc/letsencrypt/live/mail.example.com/fullchain.pem"
EXPIRY=$(openssl x509 -in "$CERT" -noout -enddate | cut -d= -f2)
EXPIRY_EPOCH=$(date -d "$EXPIRY" +%s)
NOW=$(date +%s)
DAYS_LEFT=$(( ($EXPIRY_EPOCH - $NOW) / 86400 ))

if [ $DAYS_LEFT -lt 14 ]; then
    echo "WARNING: TLS certificate expires in $DAYS_LEFT days!"
    # Send alert
fi
EOF
chmod +x /etc/cron.daily/check-tls-cert

# Enable auto-renewal
systemctl enable certbot-renew.timer
systemctl start certbot-renew.timer
```

---

### 8. Complete Server Loss

**Severity**: P0 (Critical)
**Symptoms**:
- Server unresponsive
- Hardware failure
- Data center outage
- Complete data loss

**Recovery Actions** (1-2 hours):

**Step 1: Provision new server**
```bash
# Cloud provider: Launch new instance
# Bare metal: Boot replacement server

# Minimum specs:
# - 2 CPU cores
# - 4GB RAM
# - 50GB SSD
# - Network connectivity
```

**Step 2: Install dependencies**
```bash
# Update system
apt-get update && apt-get upgrade -y

# Install Go
wget https://go.dev/dl/go1.21.linux-amd64.tar.gz
tar -C /usr/local -xzf go1.21.linux-amd64.tar.gz
export PATH=$PATH:/usr/local/bin/go

# Install build tools
apt-get install -y git make gcc sqlite3
```

**Step 3: Deploy Fune**
```bash
# Clone repository (or copy binary)
git clone https://github.com/yourorg/fune.git
cd fune
make build

# Install binaries
cp fune-server /usr/local/bin/
cp fune-admin /usr/local/bin/
chmod +x /usr/local/bin/fune-*
```

**Step 4: Restore configuration**
```bash
# Restore from version control
git clone https://github.com/yourorg/fune-config.git
cp fune-config/config.toml /etc/fune/config.toml

# Or restore from backup
aws s3 cp s3://my-backup-bucket/fune/config.toml /etc/fune/config.toml
```

**Step 5: Restore database**
```bash
# Download latest backup
aws s3 cp s3://my-backup-bucket/fune/queue_backup_latest.db.gz /var/lib/fune/

# Restore
cd /var/lib/fune
gunzip queue_backup_latest.db.gz
mv queue_backup_latest.db queue.db

# Verify integrity
sqlite3 queue.db "PRAGMA integrity_check;"

# Set permissions
chown fune:fune queue.db
chmod 640 queue.db
```

**Step 6: Configure systemd**
```bash
cat > /etc/systemd/system/fune-server.service <<EOF
[Unit]
Description=Fune SMTP Delivery Service
After=network.target

[Service]
Type=simple
User=fune
Group=fune
WorkingDirectory=/var/lib/fune
ExecStart=/usr/local/bin/fune-server -config /etc/fune/config.toml
Restart=always
RestartSec=10

[Install]
WantedBy=multi-user.target
EOF

systemctl daemon-reload
systemctl enable fune-server
systemctl start fune-server
```

**Step 7: Verify service**
```bash
# Check service status
systemctl status fune-server

# Check logs
tail -f /var/log/fune/fune.log

# Verify queue
./fune-admin queue

# Verify health
./fune-admin health

# Test HTTP API
curl -X POST http://localhost:8080/v1/messages \
    -H "Authorization: Bearer your-token" \
    -H "Content-Type: application/json" \
    -d '{"from":"test@example.com","to":"test@example.com","subject":"Test","text":"Recovery test"}'
```

**Step 8: Update DNS (if IP changed)**
```bash
# Update A/AAAA records to point to new server
# Update firewall rules
# Update load balancer configuration
```

---

### 9. Message Delivery Stall

**Severity**: P2 (Medium)
**Symptoms**:
- Queue size growing but no deliveries
- Workers idle or stuck
- No delivery attempts in logs
- Throughput drops to zero

**Immediate Actions** (10 minutes):

```bash
# 1. Check worker status
ps aux | grep fune-server
top -H -p $(pgrep fune-server)

# 2. Check queue status
./fune-admin queue

# 3. Check recent delivery attempts
./fune-admin throughput

# 4. Check for worker deadlock
pstack $(pgrep fune-server) | grep -i lock

# 5. Review logs for errors
tail -100 /var/log/fune/fune.log | grep -i "error\|worker\|delivery"
```

**Recovery Actions**:

**Restart service** (quickest):
```bash
systemctl restart fune-server

# Monitor recovery
watch -n 2 './fune-admin queue'
tail -f /var/log/fune/fune.log | grep "delivery attempt"
```

**Investigate before restart**:
```bash
# Check for resource exhaustion
netstat -an | grep ESTABLISHED | wc -l  # Connection count
lsof -p $(pgrep fune-server) | wc -l   # Open file descriptors

# Check for goroutine leak
curl http://localhost:8080/debug/pprof/goroutine?debug=2 > goroutines.txt
grep "goroutine " goroutines.txt | wc -l

# Check database locks
sqlite3 queue.db "PRAGMA lock_status;"
```

---

### 10. Callback Webhook Failure

**Severity**: P3 (Low)
**Symptoms**:
- Callbacks accumulating in queue
- Webhook endpoint unreachable
- "Failed to send callback" in logs
- Callback circuit breaker open

**Immediate Actions** (15 minutes):

```bash
# 1. Check callback queue
./fune-admin callbacks

# 2. Test webhook endpoint
curl -X POST https://your-webhook.example.com/callback \
    -H "Content-Type: application/json" \
    -H "Authorization: Bearer your-token" \
    -d '{"test":"true"}'

# 3. Check callback circuit breaker status
./fune-admin health | grep -i callback

# 4. Review recent callback failures
tail -100 /var/log/fune/fune.log | grep -i callback
```

**Recovery Actions**:

**If webhook endpoint is down**:
```bash
# Option 1: Temporarily disable callbacks
# Edit config.toml:
# [callbacks]
# webhook_url = ""  # Empty to disable

kill -SIGHUP $(cat fune.pid)

# Option 2: Use backup webhook endpoint
# Edit config.toml:
# [callbacks]
# webhook_url = "https://backup-webhook.example.com/callback"

kill -SIGHUP $(cat fune.pid)
```

**Drain accumulated callbacks** (when webhook recovers):
```bash
# Callbacks will automatically retry
# Monitor progress
watch -n 10 './fune-admin callbacks'

# Force immediate retry by restarting callback handler
systemctl restart fune-server
```

---

## Incident Response Procedures

### Incident Severity Levels

| Level | Description | Response Time | Examples |
|-------|-------------|---------------|----------|
| P0 | Critical - Service down | 15 minutes | Database corruption, server crash |
| P1 | High - Degraded service | 1 hour | High failure rate, queue overflow |
| P2 | Medium - Partial impact | 4 hours | Single component failure |
| P3 | Low - Minor issue | 24 hours | Callback delays, non-critical errors |

### Incident Response Workflow

```
1. DETECT → 2. ASSESS → 3. RESPOND → 4. RECOVER → 5. REVIEW
```

**1. Detection**:
- Monitoring alerts (Prometheus, logs)
- User reports
- Automated health checks

**2. Assessment** (5 minutes):
```bash
# Quick health check script
#!/bin/bash
echo "=== Fune Health Check ==="
echo "Service Status:"
systemctl status fune-server --no-pager

echo -e "\nQueue Status:"
./fune-admin queue

echo -e "\nThroughput:"
./fune-admin throughput

echo -e "\nRecent Errors:"
tail -50 /var/log/fune/fune.log | grep -i error

echo -e "\nSystem Resources:"
free -h
df -h
```

**3. Response**:
- Follow appropriate disaster scenario runbook
- Document actions taken
- Communicate with stakeholders

**4. Recovery**:
- Verify service restoration
- Monitor for recurrence
- Update status page

**5. Post-Incident Review**:
- Schedule within 48 hours
- Document timeline
- Identify root cause
- Create action items

---

## Communication Templates

### Initial Alert

```
INCIDENT: [P0/P1/P2/P3] - [Brief Description]

Status: INVESTIGATING
Started: [YYYY-MM-DD HH:MM UTC]
Affected: [Service/Component]

Impact: [Description of user impact]

We are investigating and will provide updates every 30 minutes.

Point of Contact: [Name] [Contact]
```

### Update Template

```
INCIDENT UPDATE - [#IncidentID]

Status: [INVESTIGATING/IDENTIFIED/MONITORING/RESOLVED]
Updated: [YYYY-MM-DD HH:MM UTC]

Current Situation:
- [What is happening now]

Actions Taken:
- [What has been done]

Next Steps:
- [What will be done next]

Next update in 30 minutes.
```

### Resolution Template

```
INCIDENT RESOLVED - [#IncidentID]

Status: RESOLVED
Resolved: [YYYY-MM-DD HH:MM UTC]
Duration: [X hours Y minutes]

Summary:
- [What happened]
- [What was done to fix it]
- [Any ongoing monitoring]

Post-Incident Review:
- Scheduled for [Date/Time]
- [Link to incident report]

Thank you for your patience.
```

---

## Post-Incident Review

### Review Template

**Incident ID**: [ID]
**Date**: [YYYY-MM-DD]
**Duration**: [X hours]
**Severity**: [P0/P1/P2/P3]

**Timeline**:
| Time | Event |
|------|-------|
| HH:MM | Incident detected |
| HH:MM | Response began |
| HH:MM | Root cause identified |
| HH:MM | Fix applied |
| HH:MM | Service restored |

**Root Cause**:
- [Technical cause]
- [Contributing factors]

**Impact**:
- Messages affected: [Count]
- Downtime: [Duration]
- Users affected: [Count/Percentage]

**What Went Well**:
- [Positive aspects]

**What Could Be Improved**:
- [Areas for improvement]

**Action Items**:
- [ ] [Action 1] - Owner: [Name] - Due: [Date]
- [ ] [Action 2] - Owner: [Name] - Due: [Date]

**Follow-up Date**: [Date]

---

## Additional Resources

- [Backup Procedures](./backups.md)
- [Monitoring Setup](./monitoring.md)
- [Configuration Reference](../config.toml.example)
- [Technical Documentation](../DOCUMENTATION.md)

---

## Quick Command Reference

```bash
# Service Management
systemctl status fune-server
systemctl restart fune-server
systemctl stop fune-server
systemctl start fune-server

# Health Checks
./fune-admin queue
./fune-admin health
./fune-admin throughput
./fune-admin failures

# Hot Reload
kill -SIGHUP $(cat fune.pid)

# Database
sqlite3 queue.db "PRAGMA integrity_check;"
sqlite3 queue.db "SELECT COUNT(*) FROM messages;"

# Logs
tail -f /var/log/fune/fune.log
journalctl -u fune-server -f

# Backup & Restore
sqlite3 queue.db ".backup 'backup.db'"
gunzip -c backup.db.gz > queue.db
```
