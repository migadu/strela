# Fune - High-Performance SMTP Delivery Service

<p align="center">
  <img src="https://raw.githubusercontent.com/migadu/fune/master/assets/fune-logo.png" alt="Fune Logo" width="200"/>
</p>

<p align="center">
  <strong>A production-ready, queue-based SMTP delivery service with direct MX delivery, intelligent retry logic, IP reputation management, and comprehensive monitoring.</strong>
</p>

<p align="center">
  <a href="https://github.com/migadu/fune/actions"><img src="https://github.com/migadu/fune/workflows/Go/badge.svg" alt="Build Status"></a>
  <a href="https://goreportcard.com/report/github.com/migadu/fune"><img src="https://goreportcard.com/badge/github.com/migadu/fune" alt="Go Report Card"></a>
  <a href="https://godoc.org/github.com/migadu/fune"><img src="https://godoc.org/github.com/migadu/fune?status.svg" alt="GoDoc"></a>
</p>

---

Fune is a modern, reliable, and high-performance email delivery service built in Go. It accepts messages via a simple HTTP API, queues them persistently, and delivers them directly to recipient MX servers. It's designed for developers and businesses who need a robust, self-hosted email sending solution without the complexity of traditional mail servers.

## Features

- **Queue-Based Architecture**: Asynchronously processes messages for high throughput and reliability.
- **Direct MX Delivery**: Delivers email directly to the recipient's mail server, bypassing relays.
- **Intelligent Retries**: Exponential backoff and greylisting handling ensure your emails get delivered.
- **IP Reputation Management**: Automatically detects and manages degraded IPs to protect your deliverability.
- **Webhook Callbacks**: Get real-time notifications for delivery events (delivered, bounced, failed).
- **DKIM Signing**: Sign your emails with DKIM for better security and deliverability.
- **Clustering Support**: Scale horizontally with optional clustering for high availability.
- **Prometheus Metrics**: In-depth monitoring of queues, deliveries, and performance.
- **Hot Reload**: Update configuration without downtime.

For a full list of features and detailed technical information, please see our [**Technical Documentation**](DOCUMENTATION.md).

## Quick Start

1.  **Build the binaries:**
    ```bash
    make all
    ```

2.  **Configure Fune:**
    Copy the example configuration and edit it to your needs.
    ```bash
    cp config.toml.example config.toml
    nano config.toml
    ```

3.  **Run the server:**
    ```bash
    ./bin/fune-server
    ```

4.  **Send a test message:**
    ```bash
    curl -X POST http://localhost:8080/v1/messages \
      -H "Authorization: Bearer your-secret-token" \
      -H "Content-Type: application/json" \
      -d 
      {
        "from": "sender@example.com",
        "to": "recipient@example.com",
        "subject": "Hello from Fune!",
        "text": "This is a test message."
      }
    ```

## Configuration

Fune is configured using a `config.toml` file. A minimal configuration looks like this:

```toml
[server]
database_path = "./queue.db"

[http]
listen = ":8080"
auth_token = "your-secret-token"

[queue]
worker_count = 10

[delivery]
source_ips = ["192.168.1.100"]
max_message_age_hours = 48

[callbacks]
webhook_url = "https://your-app.com/webhooks/delivery"
auth_token = "webhook-secret"
```

For a complete list of all configuration options, please see the [**`config.toml.example`**](config.toml.example) file.

## API

Submit a message for delivery by making a POST request to the `/v1/messages` endpoint.

**Request:**
```http
POST /v1/messages
Host: localhost:8080
Authorization: Bearer your-secret-token
Content-Type: application/json

{
  "from": "sender@example.com",
  "to": "recipient@example.com",
  "subject": "Test Subject",
  "text": "Plain text body",
  "html": "<p>HTML body</p>"
}
```

**Response (200 OK):**
```json
{
  "message_id": "msg_679d8a4c2f4h3k9d2j",
  "status": "queued",
  "queued_at": "2025-01-15T10:30:00Z"
}
```

For more details on the API, including DKIM signing and idempotency, see the [**API Usage**](DOCUMENTATION.md#api-usage) section in our documentation.

## Documentation

For detailed information about Fune's architecture, features, and deployment, please refer to the [**`DOCUMENTATION.md`**](DOCUMENTATION.md) file.

## Contributing

We welcome contributions! Please see our [contributing guidelines](CONTRIBUTING.md) for more information.

## License

Fune is licensed under the [MIT License](LICENSE).