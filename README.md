# Fune - Synchronous SMTP Delivery Gateway

Fune is a high-performance, stateless SMTP delivery service written in Go.
It accepts email messages via HTTP API and delivers them synchronously to
recipient MX servers with intelligent source IP rotation and reputation management.

## Key Features
- **Synchronous delivery** - Immediate SMTP results via JSON response
- **Stateless** - No database, no queue, no persistence
- **Concurrent** - Handles hundreds of simultaneous deliveries
- **Production-ready** - Timeout handling, rate limiting, circuit protection, Prometheus metrics

## Quick Start

### Build and Run
```bash
go build -o fune-server cmd/fune-server/main.go
./fune-server
```

### Send Email
```bash
curl -X POST http://localhost:8025/v1/deliver \
  -H "Content-Type: application/json" \
  -d '{
    "from": "sender@example.com",
    "to": "recipient@example.com",
    "subject": "Hello Fune",
    "text": "This is a test email sent via Fune."
  }'
```

Response (200 OK):
```json
{
  "status": "delivered",
  "smtp_code": 250,
  "smtp_message": "2.0.0 OK",
  "mx_host": "mx1.recipient.com",
  "source_ip": "192.168.1.100",
  "attempt_duration_ms": 1234
}
```

## Configuration

Configuration is handled via `config.toml`. See `config.toml.example` for all options.

```toml
[inbound]
listen = ":8025"
max_concurrent_requests = 200

[outbound]
delivery_timeout_seconds = 30
per_domain_interval_seconds = 2
```

## Architecture

Fune acts as a gateway between your application and external SMTP servers.

```
HTTP Request -> Fune -> MX Lookup -> SMTP Delivery -> HTTP Response
```

It handles:
- **DNS Resolution**: Caching MX lookups
- **IP Rotation**: Rotating source IPs for better deliverability
- **Rate Limiting**: Protecting destination domains from flood
- **IP Reputation**: Monitoring and avoiding degraded source IPs

## License

MIT
