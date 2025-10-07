# Security Features

This document describes the security measures implemented in Fune.

## Authentication

### Bearer Token Authentication

```toml
[inbound]
auth_token = "your-secret-token"
```

**Constant-Time Comparison**:
```go
import "crypto/subtle"

expectedToken := []byte(config.AuthToken)
providedToken := []byte(extractedToken)

if subtle.ConstantTimeCompare(expectedToken, providedToken) != 1 {
  return errors.New("invalid token")
}
```

**Why**: Prevents timing attacks. Variable-time comparison (`==`) leaks information through execution time.

## TLS/HTTPS Support

### Configuration

```toml
[inbound]
tls_enabled = true
tls_cert_file = "/path/to/cert.pem"
tls_key_file = "/path/to/key.pem"
```

**Implementation**: Uses Go's `http.Server.ListenAndServeTLS()` with provided certificate and key files.

**Certificate options**:
- **Let's Encrypt**: Free automated certificates (recommended)
- **Self-signed**: For testing/development only
- **Commercial CA**: For enterprise requirements

**Validation**: Server validates that cert and key files are specified when `tls_enabled = true`, failing fast on startup if missing.

## Request Protection

### Body Size Limits

```go
http.MaxBytesReader(w, r.Body, maxBodySizeBytes)
```

**Default**: 35 MB

**Why**: Prevents memory exhaustion from gigabyte-sized uploads.

### Rate Limiting

```toml
[inbound]
rate_limit_enabled = true
rate_limit_requests_per_ip = 100
rate_limit_window_seconds = 60
```

Per-IP token bucket rate limiting prevents API abuse.

## Message Security

### Message ID Generation

```go
import "crypto/rand"

func GenerateMessageID() string {
  b := make([]byte, 15)
  rand.Read(b)  // Cryptographically secure
  return "msg_" + base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b)
}
```

**Why crypto/rand over math/rand**: Prevents message ID prediction. Math/rand is deterministic and unsuitable for security-sensitive IDs.

### SQL Injection Prevention

All queries use parameterized statements:
```go
db.Exec("INSERT INTO messages (message_id, ...) VALUES (?, ...)", msg.MessageID, ...)
// Never: db.Exec("INSERT ... VALUES ('" + msg.MessageID + "')")
```

**Why**: Prepared statements prevent SQL injection. User input never concatenated into queries.

## SMTP Security

### Opportunistic STARTTLS

```go
// Opportunistic STARTTLS
if err := client.StartTLS(&tls.Config{
  ServerName: mxHost,
}); err != nil {
  // Log but continue (not all servers support TLS)
}
```

**Why opportunistic**: RFC 3207 specifies STARTTLS as optional. Some legacy servers don't support it. We try TLS but fall back to plaintext if unavailable.

## Network Security

### Context Timeouts

All network operations have timeouts:
```go
ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
defer cancel()
conn, err := dialer.DialContext(ctx, "tcp", addr)
```

**Why**: Prevents indefinite hangs from unresponsive servers.

### Connection Limits

- **Connection timeout**: 30s (default)
- **SMTP timeout**: 60s (default)
- **Idle timeout**: 120s (default)

Prevents resource exhaustion from slowloris attacks.

## Security Headers

Fune includes comprehensive HTTP security headers. See [SECURITY_HEADERS.md](SECURITY_HEADERS.md) for details.

## Related Documentation

- [Architecture Overview](architecture.md)
- [Component Details](components.md)
- [Deployment Guide](deployment.md)
