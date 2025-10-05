# Fune - Queue-Based SMTP Delivery Service

A production-ready, queue-based SMTP delivery service with direct MX delivery, retry logic, and webhook callbacks.

## Features

- **Queue-Based Architecture**: Accepts messages via HTTP, returns immediately (202 Accepted), processes in background
- **Direct MX Delivery**: Bypasses SMTP relay, delivers directly to recipient's MX servers
- **Multiple Source IPs**: Rotate through multiple outbound IPs with configurable strategies
- **Exponential Backoff Retry**: Intelligent retry with 5min → 12h cap over 48 hours
- **Webhook Callbacks**: Notifies CloudFlare Worker on all terminal states (delivered, hard_bounce, temp_expired, expired)
- **Comprehensive Logging**: Structured JSON logging with zap
- **Graceful Shutdown**: Completes in-flight deliveries before shutting down
- **Shared-Nothing**: Each instance has its own SQLite database, scales horizontally

## Architecture

```
HTTP POST /messages
    ↓
Queue (SQLite) → Returns 202 with message_id
    ↓
Background Workers (concurrent)
    ↓
MX Lookup (cached) → Direct SMTP Delivery
    ↓
Terminal States:
  • delivered (250 OK) → Callback → Delete
  • hard_bounce (5xx) → Callback → Delete
  • temp_expired (4xx for 48h) → Callback → Delete
  • expired (timeout) → Callback → Delete
```

## Project Structure

```
fune/
├── cmd/
│   └── fune-server/          # Main application entry point
├── internal/
│   ├── callback/             # Webhook callback system
│   ├── config/               # Configuration management
│   ├── delivery/             # SMTP delivery engine
│   ├── handler/              # HTTP request handlers
│   ├── queue/                # SQLite queue operations
│   └── worker/               # Background queue processor
├── integration_test.go       # Integration tests
└── config.toml.example       # Example configuration
```

## Installation

```bash
# Clone repository
cd fune

# Install dependencies
go mod download

# Copy example config
cp config.toml.example config.toml

# Edit config with your settings
nano config.toml

# Build
go build -o bin/fune-server ./cmd/fune-server

# Run
./bin/fune-server

# Run tests
go test -v ./integration_test.go
```

## Configuration

Create `config.toml`:

```toml
[http]
listen = ":8080"
auth_token = "your-secret-token-here"

[queue]
database_path = "./queue.db"
worker_count = 10
batch_size = 5
cleanup_interval_seconds = 60

[delivery]
source_ips = ["192.168.1.100", "192.168.1.101", "192.168.1.102"]
ip_selection = "round-robin"  # Options: round-robin, random, hash-domain
mx_cache_ttl_seconds = 3600
connection_timeout_seconds = 30
smtp_timeout_seconds = 60
max_message_age_hours = 48
initial_retry_delay_seconds = 300      # 5 minutes
max_retry_delay_seconds = 43200        # 12 hours
backoff_multiplier = 2.0
greylist_retry_delay_seconds = 120     # 2 minutes for 421

[callbacks]
webhook_url = "https://worker.example.com/api/delivery-event"
auth_token = "webhook-secret-token"
timeout_seconds = 10
max_retries = 5
retry_delay_seconds = 30
```

## API Usage

### Submit Message

**Endpoint:** `POST /messages`

**Headers:**
- `Authorization: Bearer <token>` (if configured)
- `Content-Type: application/json` or `application/x-www-form-urlencoded`

**JSON Request:**
```json
{
  "from": "sender@example.com",
  "to": "recipient@example.com",
  "subject": "Test Subject",
  "text": "Plain text body",
  "html": "<p>HTML body</p>"
}
```

**Form Request:**
```bash
curl -X POST http://localhost:8080/messages \
  -H "Authorization: Bearer your-token" \
  -H "Content-Type: application/x-www-form-urlencoded" \
  -d "from=sender@example.com" \
  -d "to=recipient@example.com" \
  -d "subject=Test" \
  -d "text=Hello World"
```

**Response (202 Accepted):**
```json
{
  "message_id": "msg_679d8a4c2f4h3k9d2j",
  "status": "queued",
  "queued_at": "2025-01-15T10:30:00Z"
}
```

### Webhook Callbacks

Fune sends POST requests to your configured `webhook_url` for all terminal states:

**Delivered:**
```json
{
  "message_id": "msg_679d8a4c2f4h3k9d2j",
  "event": "delivered",
  "email": "recipient@example.com",
  "from": "sender@example.com",
  "subject": "Test",
  "delivered_at": "2025-01-15T10:35:00Z",
  "attempts": 1,
  "smtp_code": 250,
  "smtp_response": "OK",
  "final_mx_host": "mx1.example.com",
  "source_ip": "192.168.1.100"
}
```

**Hard Bounce (5xx):**
```json
{
  "message_id": "msg_abc123",
  "event": "hard_bounce",
  "email": "invalid@example.com",
  "from": "sender@example.com",
  "subject": "Test",
  "delivered_at": "2025-01-15T10:35:00Z",
  "attempts": 1,
  "smtp_code": 550,
  "smtp_response": "User not found",
  "final_mx_host": "mx1.example.com",
  "source_ip": "192.168.1.100",
  "reason": "User not found"
}
```

**Temp Expired (4xx for 48 hours):**
```json
{
  "message_id": "msg_xyz789",
  "event": "temp_expired",
  "email": "busy@example.com",
  "from": "sender@example.com",
  "attempts": 15,
  "reason": "Mailbox busy or unavailable"
}
```

**Expired (timeout):**
```json
{
  "message_id": "msg_def456",
  "event": "expired",
  "email": "timeout@example.com",
  "attempts": 10,
  "reason": "delivery_timeout"
}
```

## Retry Schedule

Messages are retried with exponential backoff:

| Attempt | Delay | Total Elapsed |
|---------|-------|---------------|
| 1 | Immediate | 0 |
| 2 | 5 min | 5 min |
| 3 | 10 min | 15 min |
| 4 | 20 min | 35 min |
| 5 | 40 min | 1h 15min |
| 6 | 80 min | 2h 35min |
| 7 | 160 min | 5h 15min |
| 8+ | 12 hours (cap) | until 48h |

**Special Cases:**
- **Greylisting (421):** Aggressive 2-minute retry
- **Permanent (5xx):** No retry, immediate callback
- **Expired:** Messages older than 48h are not retried

## Source IP Rotation

Three strategies available:

1. **round-robin**: Cycles through IPs sequentially
2. **random**: Selects random IP for each delivery
3. **hash-domain**: Consistent IP per domain (same domain always gets same IP)

## Error Classification

- **Temporary (4xx)**: Retry with backoff
  - 450: Mailbox busy
  - 451: Local error, rate limiting
  - 452: Insufficient storage
  - 454: TLS failed

- **Permanent (5xx)**: Hard bounce, deactivate email
  - 550: User not found, mailbox unavailable
  - 551: User not local
  - 552: Message too large
  - 553: Invalid mailbox
  - 554: Transaction failed

- **Network**: DNS failures, connection errors → Retry
- **Greylist (421)**: Temporary delay → Aggressive retry

## Monitoring

All events are logged in structured JSON format:

```json
{
  "level":"info",
  "ts":1705315800.123,
  "msg":"message delivered successfully",
  "message_id":"msg_679d8a4c2f4h3k9d2j",
  "to":"recipient@example.com",
  "mx_host":"mx1.example.com",
  "source_ip":"192.168.1.100",
  "attempts":1,
  "duration_ms":1523
}
```

## Deployment

### Multi-Instance Setup

Run multiple instances behind a load balancer:

```
┌─────────────┐
│   Nginx     │ (round-robin)
└──────┬──────┘
       │
   ┌───┴───┬───────┬───────┐
   │       │       │       │
┌──▼──┐ ┌──▼──┐ ┌──▼──┐ ┌──▼──┐
│Inst1│ │Inst2│ │Inst3│ │Inst4│
└──┬──┘ └──┬──┘ └──┬──┘ └──┬──┘
   │       │       │       │
queue1.db queue2.db queue3.db queue4.db
```

Each instance:
- Has its own SQLite database
- Processes its own queue independently
- No coordination needed between instances

### systemd Service

Create `/etc/systemd/system/fune.service`:

```ini
[Unit]
Description=Fune SMTP Delivery Service
After=network.target

[Service]
Type=simple
User=fune
WorkingDirectory=/opt/fune
ExecStart=/opt/fune/fune
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
```

```bash
sudo systemctl enable fune
sudo systemctl start fune
sudo systemctl status fune
```

## Requirements

- Go 1.23+
- Outbound SMTP access (port 25)
- Multiple IP addresses (optional, for IP rotation)
- Proper DNS setup:
  - PTR (reverse DNS) records for all source IPs
  - SPF records for sender domains
  - DKIM signing (future enhancement)

## Testing

Run all tests:

```bash
go test -v ./...
```

Run with coverage:

```bash
go test -v -cover ./...
```

Current test coverage: **127 unit tests** across all components.

## License

[Your License Here]

## Support

For issues and questions, please open an issue on GitHub.
