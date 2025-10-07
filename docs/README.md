# Fune Documentation

Welcome to the Fune documentation. This directory contains comprehensive guides for understanding, deploying, and operating the Fune SMTP delivery service.

## Quick Start

**New to Fune?** Start here:
1. [Architecture Overview](architecture.md) - Understand the system design
2. [Deployment Guide](deployment.md) - Get Fune running in production
3. [Monitoring Guide](monitoring.md) - Set up observability

## Documentation Structure

### Core Concepts

- **[Architecture Overview](architecture.md)**
  System design, component flow, design philosophy, and performance characteristics

- **[Component Details](components.md)**
  Deep dive into each component: HTTP Handler, Queue, Workers, Delivery Engine, DNS Resolver, Callbacks

- **[Security Features](security.md)**
  Authentication, TLS, rate limiting, message security, and SQL injection prevention

### Error Handling & Reliability

- **[Error Handling & Retry Logic](error-handling.md)**
  Error classification, exponential backoff, greylisting, and message expiration

- **[Circuit Breaker & Failover](circuit-breaker.md)**
  Circuit breaker pattern, source IP failover, and integration with IP reputation

- **[IP Reputation Tracking](ip-reputation.md)**
  Automatic IP reputation management, blacklist checking, and recovery procedures

### Operations

- **[Deployment Guide](deployment.md)**
  System requirements, installation, configuration, systemd setup, TLS setup, DNS, and production checklist

- **[Monitoring Guide](monitoring.md)**
  Prometheus metrics, structured logging, health checks, admin CLI tools, and Grafana dashboards

- **[Backup & Restore](backups.md)**
  Backup strategies, restore procedures, disaster recovery scenarios, and automation scripts

- **[Disaster Recovery](disaster-recovery.md)**
  Comprehensive runbooks for 10 disaster scenarios, incident response, and communication templates

- **[Security Headers](SECURITY_HEADERS.md)**
  HTTP security headers implementation and best practices

### Development

- **[Testing Guide](testing.md)**
  Running tests, writing tests, test helpers, mock servers, and CI/CD integration

## Common Tasks

### Deployment

```bash
# Install from binary
wget https://github.com/yourorg/fune/releases/latest/fune-linux-amd64.tar.gz
tar xzf fune-linux-amd64.tar.gz
sudo cp fune-server fune-admin /usr/local/bin/

# Configure
sudo cp config.toml.example /etc/fune/config.toml
sudo vim /etc/fune/config.toml

# Start service
sudo systemctl start fune-server
```

See [Deployment Guide](deployment.md) for complete instructions.

### Monitoring

```bash
# Check health
./fune-admin health

# View queue stats
./fune-admin queue

# Check database health
./fune-admin database

# View IP reputation
./fune-admin reputation
```

See [Monitoring Guide](monitoring.md) for metrics and dashboards.

### Troubleshooting

Common issues:

- **Queue building up**: Check [Circuit Breaker](circuit-breaker.md)
- **High bounce rate**: Check [IP Reputation](ip-reputation.md)
- **Database full**: See [Disaster Recovery](disaster-recovery.md#2-queue-overflow--disk-full)
- **Service crash**: See [Disaster Recovery](disaster-recovery.md#3-service-crash--oom-kill)

Full troubleshooting: [Disaster Recovery Guide](disaster-recovery.md)

## API Reference

### HTTP API

**Submit Message**:
```bash
POST /v1/messages
Authorization: Bearer YOUR-TOKEN
Content-Type: application/json

{
  "from_address": "sender@example.com",
  "to_address": "recipient@example.com",
  "subject": "Test Email",
  "raw_message": "From: sender@example.com\r\n..."
}
```

**Response**: `202 Accepted`

See README.md for complete API documentation.

### Admin CLI

```bash
# Queue management
./fune-admin queue              # Show queue stats
./fune-admin queue-domains      # Top domains
./fune-admin queue-senders      # Top senders

# Monitoring
./fune-admin throughput         # Delivery throughput
./fune-admin failures           # Recent failures
./fune-admin database           # Database health

# Operations
./fune-admin health             # Health status
./fune-admin reputation         # IP reputation
./fune-admin callbacks          # Callback queue
```

## Configuration Reference

See [config.toml.example](../config.toml.example) for complete configuration options.

**Key sections**:
- `[inbound]` - HTTP API configuration
- `[outbound]` - SMTP delivery configuration
- `[queue]` - Queue and worker settings
- `[dns]` - DNS resolver configuration
- `[callbacks]` - Webhook configuration
- `[tls]` - TLS certificate management
- `[metrics]` - Prometheus metrics
- `[reputation]` - IP reputation tracking

## Architecture Diagrams

### Component Flow
```
HTTP Request → Handler → Queue (SQLite) → Worker Pool → Delivery Engine → MX Servers
                              ↓                                ↓
                         Callback Queue ← Callback Handler ← Delivery Result
```

### Message States
```
queued → sending → delivered
              → hard_bounce
              → temp_expired (retry)
              → expired
```

See [Architecture Overview](architecture.md) for detailed diagrams.

## Performance Tuning

### Worker Count

```toml
[queue]
worker_count = 20  # Increase for higher throughput
```

Recommended:
- 2 cores: 10 workers
- 4 cores: 20 workers
- 8 cores: 40 workers

### Database Performance

```bash
# Check fragmentation
./fune-admin database

# Vacuum if >30% fragmented
sqlite3 /var/lib/fune/queue.db "VACUUM;"
```

See [Monitoring Guide](monitoring.md#database-metrics) for performance metrics.

## Security Best Practices

1. **Enable TLS**: Always use HTTPS in production
2. **Strong auth token**: Use cryptographically random token
3. **Rate limiting**: Enable per-IP rate limiting
4. **Firewall**: Restrict metrics endpoint to internal network
5. **Regular updates**: Keep Fune updated for security patches

See [Security Features](security.md) for comprehensive security guide.

## Support & Resources

- **GitHub Issues**: https://github.com/yourorg/fune/issues
- **Discussions**: https://github.com/yourorg/fune/discussions
- **Changelog**: [CHANGELOG.md](../CHANGELOG.md)
- **License**: [LICENSE](../LICENSE)

## Contributing

See [Contributing Guidelines](../CONTRIBUTING.md) and [Testing Guide](testing.md).

## Document Changelog

- **2025-10-07**: Split from monolithic DOCUMENTATION.md into topic-based documents
- **2025-10-07**: Added database monitoring documentation
- **2025-10-07**: Added disaster recovery runbooks
- **2025-10-07**: Added backup and restore procedures

---

**Navigation**: [↑ Back to Top](#fune-documentation) | [Repository Root](../README.md)
