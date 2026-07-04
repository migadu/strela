package delivery

import (
	"context"
	"io"
	"log/slog"
	"net"
	"strconv"
	"testing"
	"time"

	"strela/internal/config"
)

// newDialTestDeliverer builds a Deliverer with no source IPs, pointed at the
// given SMTP port, suitable for exercising dialAndHello's error classification.
func newDialTestDeliverer(t *testing.T, smtpPort int) *Deliverer {
	t.Helper()
	cfg := &config.OutboundConfig{
		ConnectionTimeoutSeconds: 5,
		BannerTimeoutSeconds:     5,
		HandshakeTimeoutSeconds:  5,
		SMTPTimeoutSeconds:       10,
		MaxTotalDeliverySeconds:  30,
		SMTPPort:                 smtpPort,
		HelloHostname:            "test.example.com",
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	mxLookup := NewMXLookup(&config.DNSConfig{TimeoutSeconds: 1}, logger)
	return NewDeliverer(cfg, &config.ExpandedSourceIPs{}, mxLookup, logger, &config.ReputationConfig{}, nil, nil)
}

// closedLoopbackPort returns a loopback port with nothing listening on it, so a
// dial reliably fails with "connection refused" without touching the network.
func closedLoopbackPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_, portStr, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatal(err)
	}
	ln.Close() // nothing listens here now → dials get refused
	return port
}

// TestMXFallback_ConnectionRefusedIsTempFail verifies that a refused connection
// is classified as "temp_fail". Refused is a definitive per-attempt outcome
// (the host answered, it just isn't accepting), distinct from a timeout.
func TestMXFallback_ConnectionRefusedIsTempFail(t *testing.T) {
	port := closedLoopbackPort(t)
	deliverer := newDialTestDeliverer(t, port)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, result, err := deliverer.dialAndHello(ctx, testLogger(), "test-trace-id",
		"127.0.0.1",           // mxHost
		[]string{"127.0.0.1"}, // mxIPs: pre-resolved addresses to try
		"",                    // no source IP (system default routing)
		false,                 // preferIPv6
		config.ProtocolSMTP,
		deliverer.config,
	)

	if err == nil {
		t.Fatal("expected an error dialing a closed port, got nil")
	}
	if result.Status != "temp_fail" {
		t.Errorf("expected status \"temp_fail\" for connection refused, got %q (error: %v)", result.Status, err)
	}
}

// TestMXFallback_DeadlineExceededIsTimeout verifies that when the delivery
// context deadline is exceeded during the dial, the attempt is classified as
// "timeout". The delivery loop treats "timeout" as retryable and continues to
// the next MX host, so a blown deadline on one MX must not stop MX fallback.
func TestMXFallback_DeadlineExceededIsTimeout(t *testing.T) {
	port := closedLoopbackPort(t)
	deliverer := newDialTestDeliverer(t, port)

	// Deadline firmly in the past → the dial fails with a context deadline error
	// immediately, deterministically, without depending on network timing.
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Hour))
	defer cancel()

	_, result, err := deliverer.dialAndHello(ctx, testLogger(), "test-trace-id",
		"127.0.0.1",
		[]string{"127.0.0.1"},
		"",
		false,
		config.ProtocolSMTP,
		deliverer.config,
	)

	if err == nil {
		t.Fatal("expected an error when the context deadline is exceeded, got nil")
	}
	if result.Status != "timeout" {
		t.Errorf("expected status \"timeout\" for deadline exceeded (retryable, allows MX fallback), got %q (error: %v)", result.Status, err)
	}
}
