# Testing Guide

This document describes how to test Fune components and write new tests.

## Running Tests

### All Tests

```bash
# Run all tests
go test ./...

# Verbose output
go test -v ./...

# With race detection
go test -race ./...

# Generate coverage report
make coverage
```

### Specific Packages

```bash
# Test queue package
go test ./internal/queue/...

# Test delivery package
go test ./internal/delivery/...

# Test specific function
go test -run TestQueue_Enqueue ./internal/queue/...
```

### Integration Tests

```bash
# Run integration tests only
go test -v . -run Integration

# Skip integration tests
go test -v ./... -short
```

## Test Structure

### Unit Tests

Tests are located alongside source files with `_test.go` suffix.

**Example**: [internal/queue/queue_test.go](../internal/queue/queue_test.go)

```go
package queue

import (
    "testing"
    "time"
)

func TestQueue_Enqueue(t *testing.T) {
    // Setup
    q, cleanup := SetupTestQueue(t)
    defer cleanup()

    // Create test data
    msg := &QueuedMessage{
        MessageID:  "msg_test123",
        FromAddr:   "sender@example.com",
        ToAddr:     "recipient@example.com",
        ToDomain:   "example.com",
        Subject:    "Test",
        RawMessage: []byte("Test body"),
        ExpiresAt:  time.Now().Add(48 * time.Hour),
    }

    // Execute
    err := q.Enqueue(msg)

    // Assert
    if err != nil {
        t.Fatalf("Enqueue failed: %v", err)
    }

    // Verify
    retrieved, err := q.GetMessage("msg_test123")
    if retrieved == nil {
        t.Fatal("Message not found")
    }
}
```

### Integration Tests

Integration tests verify multiple components working together.

**Example**: [integration_test.go](../integration_test.go)

```go
func TestFullMessageLifecycle(t *testing.T) {
    // Create mock SMTP server
    smtpServer := startMockSMTPServer(t)
    defer smtpServer.Close()

    // Create mock webhook server
    webhookServer := startMockWebhookServer(t)
    defer webhookServer.Close()

    // Setup components
    queue := setupQueue(t)
    deliverer := setupDeliverer(t, queue)
    worker := setupWorker(t, queue, deliverer)

    // Submit message via HTTP
    submitMessage(t, httpClient, message)

    // Wait for delivery
    waitForDelivery(t, queue, timeout)

    // Verify webhook called
    verifyWebhook(t, webhookServer)
}
```

## Test Helpers

### Queue Test Setup

```go
func SetupTestQueue(t *testing.T) (*Queue, func()) {
    t.Helper()

    dbPath := "test_queue_" + time.Now().Format("20060102150405") + ".db"
    logger, _ := zap.NewDevelopment()

    queue, err := NewQueue(dbPath, logger)
    if err != nil {
        t.Fatalf("Failed to create queue: %v", err)
    }

    cleanup := func() {
        queue.Close()
        os.Remove(dbPath)
        os.Remove(dbPath + "-shm")
        os.Remove(dbPath + "-wal")
    }

    return queue, cleanup
}
```

### Mock SMTP Server

```go
func startMockSMTPServer(t *testing.T) *SMTPServer {
    t.Helper()

    listener, _ := net.Listen("tcp", "127.0.0.1:0")
    server := &SMTPServer{listener: listener}

    go func() {
        for {
            conn, _ := listener.Accept()
            handleSMTPConnection(conn)
        }
    }()

    return server
}

func handleSMTPConnection(conn net.Conn) {
    defer conn.Close()

    // SMTP protocol
    fmt.Fprintf(conn, "220 localhost ESMTP\r\n")

    scanner := bufio.NewScanner(conn)
    for scanner.Scan() {
        line := scanner.Text()

        if strings.HasPrefix(line, "EHLO") {
            fmt.Fprintf(conn, "250 localhost\r\n")
        } else if strings.HasPrefix(line, "MAIL FROM") {
            fmt.Fprintf(conn, "250 OK\r\n")
        } else if strings.HasPrefix(line, "RCPT TO") {
            fmt.Fprintf(conn, "250 OK\r\n")
        } else if line == "DATA" {
            fmt.Fprintf(conn, "354 Send message\r\n")
        } else if line == "." {
            fmt.Fprintf(conn, "250 OK\r\n")
        } else if line == "QUIT" {
            fmt.Fprintf(conn, "221 Bye\r\n")
            break
        }
    }
}
```

### Mock Webhook Server

```go
func startMockWebhookServer(t *testing.T) *httptest.Server {
    t.Helper()

    received := make([]map[string]interface{}, 0)

    handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        var payload map[string]interface{}
        json.NewDecoder(r.Body).Decode(&payload)
        received = append(received, payload)
        w.WriteHeader(http.StatusOK)
    })

    server := httptest.NewServer(handler)
    server.received = &received  // Store reference for assertions

    return server
}
```

## Test Conventions

### 1. Table-Driven Tests

For testing multiple scenarios:

```go
func TestErrorClassifier(t *testing.T) {
    tests := []struct {
        name     string
        smtpCode int
        response string
        want     ErrorCategory
    }{
        {"Temporary failure", 450, "Try again", ErrorTemporary},
        {"Permanent failure", 550, "No such user", ErrorPermanent},
        {"Greylisting", 421, "Try later", ErrorGreylist},
        {"Success", 250, "OK", ErrorNone},
    }

    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            got := ClassifyError(tt.smtpCode, tt.response)
            if got != tt.want {
                t.Errorf("got %v, want %v", got, tt.want)
            }
        })
    }
}
```

### 2. Subtests

Group related tests:

```go
func TestDelivery(t *testing.T) {
    t.Run("IPv6", func(t *testing.T) {
        // Test IPv6 delivery
    })

    t.Run("IPv4 fallback", func(t *testing.T) {
        // Test IPv4 fallback
    })

    t.Run("Multiple MX", func(t *testing.T) {
        // Test MX failover
    })
}
```

### 3. Test Fixtures

Use temporary directories:

```go
func TestFileOperation(t *testing.T) {
    tmpDir := t.TempDir()  // Automatically cleaned up

    filePath := filepath.Join(tmpDir, "test.txt")
    // ... test file operations
}
```

### 4. Timeout Protection

Prevent hanging tests:

```go
func TestWithTimeout(t *testing.T) {
    done := make(chan bool)

    go func() {
        // Long-running operation
        done <- true
    }()

    select {
    case <-done:
        // Success
    case <-time.After(5 * time.Second):
        t.Fatal("Test timed out")
    }
}
```

## Continuous Integration

### GitHub Actions

Example workflow (`.github/workflows/test.yml`):

```yaml
name: Test

on: [push, pull_request]

jobs:
  test:
    runs-on: ubuntu-latest

    steps:
      - uses: actions/checkout@v3

      - name: Set up Go
        uses: actions/setup-go@v4
        with:
          go-version: '1.21'

      - name: Build
        run: make build

      - name: Run tests
        run: go test -v -race -coverprofile=coverage.txt ./...

      - name: Upload coverage
        uses: codecov/codecov-action@v3
        with:
          files: ./coverage.txt
```

## Writing New Tests

### Checklist

When adding a new feature:

1. [ ] Write unit tests for new functions
2. [ ] Test error cases and edge conditions
3. [ ] Test concurrent access if applicable
4. [ ] Add integration test if multiple components involved
5. [ ] Update existing tests if behavior changes
6. [ ] Verify tests pass: `go test ./...`
7. [ ] Check coverage: `make coverage`

### Example: Testing New Component

```go
// 1. Create test file: internal/newcomponent/newcomponent_test.go

package newcomponent

import (
    "testing"
)

// 2. Test creation
func TestNew(t *testing.T) {
    comp := New(config)
    if comp == nil {
        t.Fatal("New() returned nil")
    }
}

// 3. Test main functionality
func TestMainFunction(t *testing.T) {
    comp := setupComponent(t)

    result, err := comp.MainFunction(input)

    if err != nil {
        t.Fatalf("MainFunction() failed: %v", err)
    }

    if result != expected {
        t.Errorf("got %v, want %v", result, expected)
    }
}

// 4. Test error cases
func TestMainFunction_Error(t *testing.T) {
    comp := setupComponent(t)

    _, err := comp.MainFunction(invalidInput)

    if err == nil {
        t.Error("expected error, got nil")
    }
}

// 5. Test concurrent access
func TestMainFunction_Concurrent(t *testing.T) {
    comp := setupComponent(t)

    var wg sync.WaitGroup
    for i := 0; i < 10; i++ {
        wg.Add(1)
        go func() {
            defer wg.Done()
            comp.MainFunction(input)
        }()
    }

    wg.Wait()
}

// Helper
func setupComponent(t *testing.T) *Component {
    t.Helper()
    // Setup logic
    return &Component{}
}
```

## Benchmarking

Test performance:

```go
func BenchmarkEnqueue(b *testing.B) {
    q, cleanup := SetupTestQueue(b)
    defer cleanup()

    msg := &QueuedMessage{/* ... */}

    b.ResetTimer()
    for i := 0; i < b.N; i++ {
        q.Enqueue(msg)
    }
}
```

Run benchmarks:
```bash
go test -bench=. -benchmem ./internal/queue/...
```

## Related Documentation

- [Architecture Overview](architecture.md)
- [Component Details](components.md)
- [Contributing Guidelines](../CONTRIBUTING.md) (if exists)
