# Deployment Guide

This document provides guidance for deploying Fune in production environments.

## System Requirements

### Minimum Requirements

- **CPU**: 2 cores
- **RAM**: 2GB
- **Disk**: 20GB SSD (for database and logs)
- **OS**: Linux (Ubuntu 20.04+, Debian 11+, RHEL 8+)
- **Go**: 1.21 or later (for building from source)

### Recommended Production

- **CPU**: 4+ cores
- **RAM**: 4-8GB
- **Disk**: 50GB+ SSD with 20%+ free space
- **Network**: Static public IPv4/IPv6 addresses
- **Monitoring**: Prometheus + Grafana

## Installation

### Option 1: Binary Release

```bash
# Download latest release
wget https://github.com/yourorg/fune/releases/download/v1.0.0/fune-linux-amd64.tar.gz
tar xzf fune-linux-amd64.tar.gz

# Install binaries
sudo cp fune-server fune-admin /usr/local/bin/
sudo chmod +x /usr/local/bin/fune-*

# Verify installation
fune-server --version
```

### Option 2: Build from Source

```bash
# Install Go 1.21+
wget https://go.dev/dl/go1.21.linux-amd64.tar.gz
sudo tar -C /usr/local -xzf go1.21.linux-amd64.tar.gz
export PATH=$PATH:/usr/local/go/bin

# Clone repository
git clone https://github.com/yourorg/fune.git
cd fune

# Build
make build

# Install
sudo cp fune-server fune-admin /usr/local/bin/
```

## Configuration

### 1. Create Configuration File

```bash
sudo mkdir -p /etc/fune
sudo cp config.toml.example /etc/fune/config.toml
sudo chown fune:fune /etc/fune/config.toml
sudo chmod 640 /etc/fune/config.toml
```

### 2. Edit Configuration

```toml
[server]
database_path = "/var/lib/fune/queue.db"
log_level = "info"

[inbound]
listen = ":8080"
auth_token = "CHANGE-THIS-SECRET-TOKEN"
tls_enabled = true
tls_cert_file = "/etc/letsencrypt/live/mail.example.com/fullchain.pem"
tls_key_file = "/etc/letsencrypt/live/mail.example.com/privkey.pem"
rate_limit_enabled = true
rate_limit_requests_per_ip = 100

[outbound]
source_ips = ["YOUR.PUBLIC.IP.V4", "YOUR:PUBLIC:IPV6::ADDR"]
source_ip_selection = "round-robin"
circuit_breaker_enabled = true

[callbacks]
webhook_url = "https://your-app.example.com/webhooks/email"

[metrics]
enabled = true
listen = ":9090"

[health]
enabled = true
path = "/health"
```

### 3. Create User and Directories

```bash
# Create fune user
sudo useradd --system --no-create-home --shell /bin/false fune

# Create data directories
sudo mkdir -p /var/lib/fune
sudo mkdir -p /var/log/fune

# Set permissions
sudo chown -R fune:fune /var/lib/fune /var/log/fune
sudo chmod 750 /var/lib/fune /var/log/fune
```

## Systemd Service

### Service File

Create `/etc/systemd/system/fune-server.service`:

```ini
[Unit]
Description=Fune SMTP Delivery Service
After=network.target
Documentation=https://github.com/yourorg/fune

[Service]
Type=simple
User=fune
Group=fune
WorkingDirectory=/var/lib/fune

ExecStart=/usr/local/bin/fune-server -config /etc/fune/config.toml

# Restart on failure
Restart=always
RestartSec=10
StartLimitBurst=5
StartLimitIntervalSec=300

# Security
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=true
ReadWritePaths=/var/lib/fune /var/log/fune

# Resource limits
MemoryMax=2G
MemoryHigh=1.5G
TasksMax=100

# Logging
StandardOutput=journal
StandardError=journal
SyslogIdentifier=fune-server

[Install]
WantedBy=multi-user.target
```

### Enable and Start

```bash
# Reload systemd
sudo systemctl daemon-reload

# Enable autostart
sudo systemctl enable fune-server

# Start service
sudo systemctl start fune-server

# Check status
sudo systemctl status fune-server

# View logs
sudo journalctl -u fune-server -f
```

## TLS/HTTPS Setup

### Option 1: Let's Encrypt (Recommended)

```bash
# Install certbot
sudo apt-get install certbot

# Obtain certificate
sudo certbot certonly --standalone -d mail.example.com

# Certificates will be at:
# /etc/letsencrypt/live/mail.example.com/fullchain.pem
# /etc/letsencrypt/live/mail.example.com/privkey.pem

# Allow fune to read certificates
sudo chmod 755 /etc/letsencrypt/{live,archive}
sudo chmod 644 /etc/letsencrypt/live/mail.example.com/*.pem

# Auto-renewal (certbot sets this up automatically)
sudo certbot renew --dry-run
```

**Hot Reload After Renewal**:
```bash
# Fune automatically reloads certificates, or manually:
sudo systemctl reload fune-server
```

### Option 2: Commercial Certificate

```bash
# Copy certificates
sudo cp your-cert.pem /etc/fune/cert.pem
sudo cp your-key.pem /etc/fune/key.pem
sudo chown fune:fune /etc/fune/*.pem
sudo chmod 640 /etc/fune/*.pem
```

Update `config.toml`:
```toml
[inbound]
tls_cert_file = "/etc/fune/cert.pem"
tls_key_file = "/etc/fune/key.pem"
```

## DNS Configuration

### Required DNS Records

```dns
; A record for API endpoint
mail.example.com.  IN  A     YOUR.PUBLIC.IP.V4

; AAAA record for IPv6
mail.example.com.  IN  AAAA  YOUR:PUBLIC:IPV6::ADDR

; SPF record for your sending domain
example.com.  IN  TXT  "v=spf1 ip4:YOUR.PUBLIC.IP.V4 ip6:YOUR:PUBLIC:IPV6::ADDR -all"

; DKIM record (if using DKIM signing)
default._domainkey.example.com.  IN  TXT  "v=DKIM1; k=rsa; p=YOUR_PUBLIC_KEY"
```

### Verify DNS

```bash
# Test A record
dig +short mail.example.com A

# Test AAAA record
dig +short mail.example.com AAAA

# Test SPF
dig +short example.com TXT | grep spf1
```

## Firewall Configuration

```bash
# Allow HTTPS (API)
sudo ufw allow 8080/tcp

# Allow Prometheus metrics (internal only)
sudo ufw allow from 10.0.0.0/8 to any port 9090

# Allow outbound SMTP
sudo ufw allow out 25/tcp

# Enable firewall
sudo ufw enable
```

## Monitoring Setup

### Prometheus Configuration

Add to `prometheus.yml`:

```yaml
scrape_configs:
  - job_name: 'fune'
    static_configs:
      - targets: ['localhost:9090']
    scrape_interval: 15s
```

### Grafana Dashboard

Import dashboard from `grafana-dashboard.json` or create panels for:
- Queue depth
- Delivery success rate
- Circuit breaker state
- HTTP request rate
- Database size

## Backup Strategy

### Automated Backups

```bash
# Create backup script
sudo cat > /usr/local/bin/fune-backup.sh <<'EOF'
#!/bin/bash
set -e

BACKUP_DIR="/var/backups/fune"
DB_PATH="/var/lib/fune/queue.db"
RETENTION_DAYS=30

mkdir -p "$BACKUP_DIR"

# Create backup with timestamp
TIMESTAMP=$(date +%Y%m%d_%H%M%S)
sqlite3 "$DB_PATH" ".backup '$BACKUP_DIR/queue_backup_$TIMESTAMP.db'"

# Compress
gzip "$BACKUP_DIR/queue_backup_$TIMESTAMP.db"

# Upload to S3 (optional)
# aws s3 cp "$BACKUP_DIR/queue_backup_$TIMESTAMP.db.gz" s3://my-backup-bucket/fune/

# Remove old backups
find "$BACKUP_DIR" -name "queue_backup_*.db.gz" -mtime +$RETENTION_DAYS -delete

echo "Backup completed: queue_backup_$TIMESTAMP.db.gz"
EOF

sudo chmod +x /usr/local/bin/fune-backup.sh
```

**Schedule Backups**:
```bash
# Add to crontab
sudo crontab -e

# Backup every 6 hours
0 */6 * * * /usr/local/bin/fune-backup.sh >> /var/log/fune/backup.log 2>&1
```

See [Backup & Restore Guide](backups.md) for detailed procedures.

## Production Checklist

- [ ] Static public IP addresses configured
- [ ] DNS records (A, AAAA, SPF) configured
- [ ] TLS certificate installed and auto-renewing
- [ ] Authentication token changed from default
- [ ] Rate limiting enabled
- [ ] Circuit breaker enabled
- [ ] Multiple source IPs configured (3+)
- [ ] Firewall rules configured
- [ ] Systemd service enabled and running
- [ ] Backup script scheduled
- [ ] Monitoring/alerting configured
- [ ] Log aggregation configured
- [ ] Health checks configured
- [ ] IP reputation monitoring setup
- [ ] Webhook endpoint configured and tested
- [ ] Database size monitoring enabled
- [ ] Disaster recovery plan documented

## Scaling Considerations

### Vertical Scaling

Fune scales vertically on a single instance:

```toml
[queue]
worker_count = 20  # Increase workers for higher throughput
batch_size = 10    # Increase batch size
```

**Recommended worker counts**:
- 2 cores: 10 workers
- 4 cores: 20 workers
- 8 cores: 40 workers

### Horizontal Scaling

For very high volumes, deploy multiple instances:

1. **Domain-based partitioning**: Route by `hash(recipient_domain) % N`
2. **Separate SQLite per instance**: No shared database
3. **Load balancer**: Distribute with consistent hashing

```
Client → Load Balancer → [Fune Instance 1, Fune Instance 2, Fune Instance 3]
                          (separate databases, separate queues)
```

## Maintenance

### Hot Configuration Reload

```bash
# Edit configuration
sudo vim /etc/fune/config.toml

# Reload without downtime
sudo systemctl reload fune-server

# Or send SIGHUP signal
sudo kill -HUP $(cat /var/lib/fune/fune.pid)
```

### Database Maintenance

```bash
# Check database stats
./fune-admin database

# Vacuum if fragmented >30%
sqlite3 /var/lib/fune/queue.db "VACUUM;"

# Archive old delivered messages
./fune-admin cleanup --older-than 7d
```

### Log Rotation

Create `/etc/logrotate.d/fune`:

```
/var/log/fune/*.log {
    daily
    rotate 14
    compress
    delaycompress
    notifempty
    create 0640 fune fune
    sharedscripts
    postrotate
        systemctl reload fune-server
    endscript
}
```

## Troubleshooting

See [Disaster Recovery Guide](disaster-recovery.md) for comprehensive troubleshooting procedures.

**Quick diagnostics**:
```bash
# Check service status
systemctl status fune-server

# View recent logs
journalctl -u fune-server -n 100

# Check queue health
./fune-admin queue

# Check database health
./fune-admin database

# Test health endpoint
curl http://localhost:8080/health
```

## Related Documentation

- [Architecture Overview](architecture.md)
- [Security Features](security.md)
- [Monitoring Guide](monitoring.md)
- [Backup & Restore](backups.md)
- [Disaster Recovery](disaster-recovery.md)
