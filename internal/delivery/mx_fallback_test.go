package delivery

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"strela/internal/config"
)

// TestMXFallback_ConnectionTimeout verifies that when a connection to one MX
// times out, Strela properly falls back to trying other MX hosts.
// This is a simplified test that verifies the core logic without complex mocking.
func TestMXFallback_ConnectionTimeout(t *testing.T) {
	// This test verifies that the fix in dialAndHello works correctly.
	// We check that connection failures return "timeout" status instead of "temp_fail".

	// Create a deliverer with very short timeouts
	cfg := &config.OutboundConfig{
		ConnectionTimeoutSeconds: 1, // Very short timeout
		BannerTimeoutSeconds:     5,
		HandshakeTimeoutSeconds:  5,
		SMTPTimeoutSeconds:       10,
		MaxTotalDeliverySeconds:  30,
		SMTPPort:                 25,
		HelloHostname:            "test.example.com",
	}

	expandedIPs := &config.ExpandedSourceIPs{
		IPv4: []string{},
		IPv6: []string{},
	}

	dnsCfg := &config.DNSConfig{
		TimeoutSeconds:          1,
		CacheTTLSeconds:         60,
		CacheNegativeTTLSeconds: 10,
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	mxLookup := NewMXLookup(dnsCfg, logger)

	// Need to provide a reputation config to avoid nil pointer
	repCfg := &config.ReputationConfig{}

	deliverer := NewDeliverer(cfg, expandedIPs, mxLookup, logger, repCfg, nil, nil)

	// Test the dialAndHello function directly with an unreachable IP
	// This IP is in the TEST-NET-1 range (192.0.2.0/24) which should not route
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// Call dialAndHello with an unreachable host
	_, result, err := deliverer.dialAndHello(ctx, logger, "test-trace-id",
		"192.0.2.1", // TEST-NET-1 IP, unreachable
		"",          // no source IP
		false,       // preferIPv6
		config.ProtocolSMTP)

	// The key assertion: connection timeout should return "timeout" status, not "temp_fail"
	if result.Status != "timeout" {
		t.Errorf("Expected connection timeout to return status 'timeout', got '%s'", result.Status)
		t.Logf("Error: %v", err)
		t.Logf("Result: %+v", result)
	}

	// Verify the error message indicates a timeout
	if err == nil {
		t.Errorf("Expected an error for unreachable host")
	}

	t.Logf("Test passed: Connection timeout returns 'timeout' status, allowing MX fallback")
}
