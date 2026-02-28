# Fune API Examples

The Fune SMTP delivery gateway supports two API modes for submitting messages:

## 1. JSON Mode (Legacy)

Send JSON with Content-Type: `application/json`

### Basic Example

```bash
curl -X POST http://localhost:8080/deliver \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer your-token-here" \
  -d '{
    "from": "sender@example.com",
    "to": "recipient@example.com",
    "raw_message": "From: sender@example.com\r\nTo: recipient@example.com\r\nSubject: Test\r\n\r\nEmail body"
  }'
```

### JSON with DKIM/ARC Parameters

```bash
curl -X POST http://localhost:8080/deliver \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer your-token-here" \
  -d '{
    "from": "sender@example.com",
    "to": "recipient@example.com",
    "raw_message": "From: sender@example.com\r\nTo: recipient@example.com\r\nSubject: Test\r\n\r\nEmail body",
    "dkim_private_key": "-----BEGIN RSA PRIVATE KEY-----\n...",
    "dkim_selector": "default",
    "dkim_domain": "example.com",
    "arc_private_key": "-----BEGIN RSA PRIVATE KEY-----\n...",
    "arc_selector": "arc1",
    "arc_domain": "arc.example.com"
  }'
```

### JSON Request Schema

```json
{
  "from": "sender@example.com",
  "to": "recipient@example.com",
  "raw_message": "RFC822 message",
  "dkim_private_key": "optional - PEM private key",
  "dkim_selector": "optional - DKIM selector",
  "dkim_domain": "optional - DKIM domain",
  "skip_dkim_validation": false,
  "arc_private_key": "optional - PEM private key",
  "arc_selector": "optional - ARC selector",
  "arc_domain": "optional - ARC domain"
}
```

---

## 2. Header Mode (New)

Send raw RFC822 message with Content-Type: `message/rfc822` and parameters in HTTP headers.

### Basic Example

```bash
curl -X POST http://localhost:8080/deliver \
  -H "Content-Type: message/rfc822" \
  -H "Authorization: Bearer your-token-here" \
  -H "X-Envelope-From: sender@example.com" \
  -H "X-Envelope-To: recipient@example.com" \
  --data-binary @message.eml
```

### Header Mode with DKIM/ARC

**Important:** Private keys MUST be base64-encoded when sent in HTTP headers (newlines are not allowed in HTTP header values).

```bash
# Encode private keys to base64 first (remove line wrapping)
DKIM_KEY_B64=$(cat dkim-key.pem | base64 -w 0)  # Linux: -w 0
# DKIM_KEY_B64=$(cat dkim-key.pem | base64)     # macOS: no -w flag needed

curl -X POST http://localhost:8080/deliver \
  -H "Content-Type: message/rfc822" \
  -H "Authorization: Bearer your-token-here" \
  -H "X-Envelope-From: sender@example.com" \
  -H "X-Envelope-To: recipient@example.com" \
  -H "X-DKIM-Private-Key: $DKIM_KEY_B64" \
  -H "X-DKIM-Selector: default" \
  -H "X-DKIM-Domain: example.com" \
  --data-binary @message.eml
```

**JavaScript/TypeScript Example:**

```javascript
import { readFileSync } from 'fs';

const dkimPrivateKey = readFileSync('dkim-key.pem', 'utf-8');
const rawEmail = readFileSync('message.eml', 'utf-8');

// Base64 encode the private key for HTTP header
const dkimKeyB64 = Buffer.from(dkimPrivateKey).toString('base64');

const response = await fetch('http://localhost:8080/deliver', {
  method: 'POST',
  headers: {
    'Content-Type': 'message/rfc822',
    'Authorization': 'Bearer your-token-here',
    'X-Envelope-From': 'sender@example.com',
    'X-Envelope-To': 'recipient@example.com',
    'X-ARC-Private-Key': dkimKeyB64,  // Base64 encoded
    'X-ARC-Selector': 'arc1',
    'X-ARC-Domain': 'example.com'
  },
  body: rawEmail
});
```

### Header Mode Example with Inline Message

```bash
curl -X POST http://localhost:8080/deliver \
  -H "Content-Type: message/rfc822" \
  -H "Authorization: Bearer your-token-here" \
  -H "X-Envelope-From: sender@example.com" \
  -H "X-Envelope-To: recipient@example.com" \
  -d "From: sender@example.com
To: recipient@example.com
Subject: Test Email
Message-ID: <test-123@example.com>
Date: $(date -R)

This is the email body.
"
```

### Header Mode HTTP Headers

| Header | Required | Description |
|--------|----------|-------------|
| `Content-Type` | Yes | Must be `message/rfc822` |
| `Authorization` | Yes* | Bearer token (if auth enabled) |
| `X-Envelope-From` | Yes | SMTP envelope sender |
| `X-Envelope-To` | Yes | SMTP envelope recipient |
| `X-DKIM-Private-Key` | No | PEM-encoded DKIM private key |
| `X-DKIM-Selector` | No | DKIM selector |
| `X-DKIM-Domain` | No | DKIM domain |
| `X-ARC-Private-Key` | No | PEM-encoded ARC private key |
| `X-ARC-Selector` | No | ARC selector |
| `X-ARC-Domain` | No | ARC domain |

*Authentication required if `auth_token` is configured.

---

## Response Format (Both Modes)

Both API modes return the same JSON response format:

### Success (200 OK)

```json
{
  "status": "delivered",
  "smtp_code": 250,
  "smtp_message": "2.0.0 OK",
  "mx_host": "mx1.example.com",
  "source_ip": "192.0.2.1",
  "attempt_duration_ms": 1234
}
```

### Temporary Failure (429 Too Many Requests)

**SMTP 4xx errors** → HTTP 429 (temporary failure, retry with backoff)

```json
{
  "status": "temp_fail",
  "smtp_code": 450,
  "smtp_message": "4.7.1 Greylisted, please retry",
  "mx_host": "mx1.example.com",
  "source_ip": "192.0.2.1",
  "attempt_duration_ms": 2345,
  "error": "temporary failure"
}
```

### Hard Bounce (554 Transaction Failed)

**SMTP 5xx errors** → HTTP 554 (permanent failure, do not retry)

```json
{
  "status": "hard_bounce",
  "smtp_code": 550,
  "smtp_message": "5.1.1 User unknown",
  "mx_host": "mx1.example.com",
  "source_ip": "192.0.2.1",
  "attempt_duration_ms": 1567
}
```

**Note**: HTTP 554 indicates permanent SMTP failures (user unknown, domain rejected, reputation blocks, etc.). Do not retry these deliveries.

### Malformed Request (400 Bad Request)

Invalid RFC822 message, missing required headers, or validation errors:

```json
{
  "error": "Invalid 'to' address: missing '@' separator"
}
```

### Timeout (504 Gateway Timeout)

```json
{
  "status": "timeout",
  "smtp_code": 0,
  "smtp_message": "",
  "mx_host": "mx1.example.com",
  "source_ip": "192.0.2.1",
  "attempt_duration_ms": 30000,
  "error": "delivery timeout exceeded"
}
```

### HTTP Status Code Summary

| HTTP Code | SMTP Equivalent | Retry? | Description |
|-----------|----------------|--------|-------------|
| **200** | 2xx | N/A | Message delivered successfully |
| **400** | N/A | No | Malformed request (bad RFC822, missing headers) |
| **429** | 4xx | Yes | Temporary failure (greylisting, rate limits, transient errors) |
| **504** | N/A | Yes | Delivery timeout exceeded |
| **554** | 5xx | No | Permanent failure (user unknown, reputation block, policy rejection) |

### Status Values

- `delivered` - Message successfully delivered (SMTP 2xx → HTTP 200)
- `temp_fail` - Temporary failure, retry later (SMTP 4xx → HTTP 429)
- `hard_bounce` - Permanent failure, do not retry (SMTP 5xx → HTTP 554)
- `timeout` - Delivery timeout exceeded (HTTP 504)
- `error` - Internal error occurred (HTTP 500)

---

## Choosing Between Modes

### Use **JSON Mode** when:
- You're building the message programmatically
- You prefer a single JSON payload
- Migrating from existing JSON-based API

### Use **Header Mode** when:
- You already have a pre-built RFC822 message
- You're forwarding/relaying existing emails
- You prefer HTTP headers for metadata
- You want to separate message content from parameters
- You're integrating with systems that output raw MIME messages

Both modes support the same features (DKIM, ARC, SRS) and have identical delivery behavior.
