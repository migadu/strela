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

### Send Email (Composed Mode)
```bash
curl -X POST http://localhost:8025/deliver \
  -H "Content-Type: application/json" \
  -d '{
    "from": "sender@example.com",
    "to": "recipient@example.com",
    "subject": "Hello Fune",
    "text": "This is a test email sent via Fune.",
    "message_id": "<unique-id@example.com>"
  }'
```

**Note**: The `message_id` field is optional. If not provided, Fune automatically generates a unique Message-ID header to prevent the email from being flagged as spam.

### Forward Email (Raw Message Mode)
For email forwarding scenarios, send the complete RFC822 message:

```bash
curl -X POST http://localhost:8025/deliver \
  -H "Content-Type: application/json" \
  -d '{
    "from": "sender@example.com",
    "to": "recipient@example.com",
    "raw_message": "From: sender@example.com\r\nTo: recipient@example.com\r\nSubject: Forwarded Email\r\nMessage-ID: <original@upstream.com>\r\n\r\nEmail body"
  }'
```

**Raw Message Mode Features:**
- Preserves all original headers (Message-ID, Received, etc.)
- **ARC signing** automatically applied (if `[arc] enabled = true`)
- **SRS envelope rewriting** automatically applied (if `[srs] enabled = true`)
- **DKIM signing** automatically applied (if `[dkim] enabled = true`)
- Ideal for email forwarding/relay scenarios

**Processing Order for Raw Messages:**
1. Original message passed to delivery engine
2. DKIM signature added (if enabled)
3. ARC headers prepended (if enabled)
4. SRS applied to envelope sender during SMTP (if enabled)
5. Delivered to final recipient

**Dynamic ARC/DKIM Keys (Multi-Tenant Support):**
```bash
curl -X POST http://localhost:8025/deliver \
  -H "Content-Type: application/json" \
  -d '{
    "from": "sender@tenant1.com",
    "to": "recipient@example.com",
    "raw_message": "From: ...",
    "arc_private_key": "-----BEGIN RSA PRIVATE KEY-----\n...\n-----END RSA PRIVATE KEY-----",
    "arc_selector": "tenant1-arc",
    "arc_domain": "tenant1.com"
  }'
```

- **Per-request override**: API parameters override config defaults (when enabled)
- **Config fallback**: If not provided, uses `[arc]` and `[dkim]` config
- **Enabled flag required**: Config must have `enabled = true` for DKIM/ARC to be applied
- **Security**: If config `enabled = false`, all API parameters are ignored

**Note**: You cannot use both `raw_message` and composed fields (`subject`, `text`, `html`) in the same request.

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

### Send Email with DKIM Signing

**Option 1: Server-wide DKIM (Recommended)**

Configure DKIM in `config.toml` to automatically sign all outbound messages:

```toml
[dkim]
enabled = true
selector = "default"
domain = "example.com"
private_key_path = "/etc/fune/dkim-private.key"
skip_validation = false
```

Once configured, all messages are automatically signed:

```bash
curl -X POST http://localhost:8025/deliver \
  -H "Content-Type: application/json" \
  -d '{
    "from": "sender@example.com",
    "to": "recipient@example.com",
    "subject": "Hello Fune",
    "text": "This email will be automatically signed with DKIM."
  }'
```

**Option 2: Per-Request DKIM**

Override config defaults or provide DKIM parameters per-request:

```bash
curl -X POST http://localhost:8025/deliver \
  -H "Content-Type: application/json" \
  -d '{
    "from": "sender@example.com",
    "to": "recipient@example.com",
    "subject": "Hello Fune",
    "text": "This is a test email sent via Fune with DKIM.",
    "dkim_private_key": "-----BEGIN RSA PRIVATE KEY-----\nMIIE...\n-----END RSA PRIVATE KEY-----",
    "dkim_selector": "default",
    "dkim_domain": "example.com"
  }'
```

**DKIM Parameters:**
- Config `enabled = true` required: DKIM only applied if config explicitly enables it
- API request parameters override config defaults (when enabled)
- **If config `enabled = false`**: All DKIM parameters ignored (logged as warning)

**DKIM Validation**: By default, Fune validates that:
1. A DKIM TXT record exists at `selector._domainkey.domain`
2. The public key in DNS matches the provided private key

This ensures proper DKIM configuration before signing. Set `skip_dkim_validation: true` to disable validation (faster but less safe).

## Configuration

Configuration is handled via `config.toml`. See `config.toml.example` for all options.

```toml
[inbound]
listen = ":8025"
max_concurrent_requests = 200

[outbound]
delivery_timeout_seconds = 30
per_domain_interval_seconds = 2

# DKIM signing for all outbound messages
[dkim]
enabled = true
selector = "default"
domain = "example.com"
private_key_path = "/etc/fune/dkim-private.key"

# For email forwarding scenarios
[arc]
enabled = true
selector = "arc"
domain = "mail.example.com"
private_key_path = "/etc/fune/arc-private.key"

[srs]
enabled = true
domain = "mail.example.com"
secret = "your-secure-secret-min-16-chars"
```

### Email Forwarding Configuration

When using Fune as an email forwarder with `raw_message` mode:

1. **DKIM Signing** (optional but recommended):
   - Signs the forwarded message with your domain's credentials
   - Works with both raw and composed messages
   - Applied before ARC signing

2. **ARC (Authenticated Received Chain)**: Preserves authentication results across forwarding hops
   - Prevents DMARC failures on forwarded emails
   - Adds ARC-Seal, ARC-Message-Signature, and ARC-Authentication-Results headers
   - Automatically increments ARC instance number (i=1, i=2, etc.)
   - Requires RSA private key (same format as DKIM)
   - **Applied to raw messages automatically**
   - **Supports dynamic keys**: Pass `arc_private_key`, `arc_selector`, `arc_domain` in API request to override config

3. **SRS (Sender Rewriting Scheme)**: Rewrites envelope sender to prevent SPF failures
   - Automatically applied to MAIL FROM during SMTP delivery
   - Preserves original sender info in encoded format
   - Requires a strong secret (minimum 16 characters)
   - **Applied to all messages (raw and composed) automatically**

**Important**: All three features work seamlessly with `raw_message` mode. When you POST a raw RFC822 message, Fune will:
- Preserve all original headers
- Add DKIM-Signature header (if DKIM enabled)
- Prepend ARC headers (if ARC enabled)
- Rewrite envelope sender with SRS (if SRS enabled)
- Deliver to final recipient

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
