# Fune API Examples

The Fune SMTP delivery gateway supports two API modes for submitting messages:

## 1. JSON Mode (Legacy)

Send JSON with Content-Type: `application/json`

### Basic Example

```bash
curl -X POST http://localhost:8080/v1/deliver \
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
curl -X POST http://localhost:8080/v1/deliver \
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
curl -X POST http://localhost:8080/v1/deliver \
  -H "Content-Type: message/rfc822" \
  -H "Authorization: Bearer your-token-here" \
  -H "X-Envelope-From: sender@example.com" \
  -H "X-Envelope-To: recipient@example.com" \
  --data-binary @message.eml
```

### Header Mode with DKIM/ARC

```bash
curl -X POST http://localhost:8080/v1/deliver \
  -H "Content-Type: message/rfc822" \
  -H "Authorization: Bearer your-token-here" \
  -H "X-Envelope-From: sender@example.com" \
  -H "X-Envelope-To: recipient@example.com" \
  -H "X-DKIM-Private-Key: $(cat dkim-key.pem | base64)" \
  -H "X-DKIM-Selector: default" \
  -H "X-DKIM-Domain: example.com" \
  -H "X-ARC-Private-Key: $(cat arc-key.pem | base64)" \
  -H "X-ARC-Selector: arc1" \
  -H "X-ARC-Domain: arc.example.com" \
  --data-binary @message.eml
```

### Header Mode Example with Inline Message

```bash
curl -X POST http://localhost:8080/v1/deliver \
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

### Temporary Failure (422 Unprocessable Entity)

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

### Hard Bounce (400 Bad Request)

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

### Status Values

- `delivered` - Message successfully delivered (250)
- `temp_fail` - Temporary failure, retry later (4xx codes)
- `hard_bounce` - Permanent failure, do not retry (5xx codes)
- `timeout` - Delivery timeout exceeded
- `error` - Internal error occurred

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
