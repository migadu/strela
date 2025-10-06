package delivery

import (
	"context"
	"testing"
	"time"

	"fune/internal/config"

	"go.uber.org/zap"
)

func TestDNSResolver_SystemDefault(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	cfg := &config.DNSConfig{
		Resolvers:      []string{}, // Use system default
		TimeoutSeconds: 5,
	}

	resolver := NewDNSResolver(cfg, logger)

	// Test MX lookup for a known domain
	ctx := context.Background()
	records, err := resolver.LookupMX(ctx, "gmail.com")

	if err != nil {
		t.Fatalf("MX lookup failed: %v", err)
	}

	if len(records) == 0 {
		t.Fatal("No MX records returned")
	}

	t.Logf("Found %d MX records for gmail.com", len(records))
	for _, mx := range records {
		t.Logf("  MX: %s (priority %d)", mx.Host, mx.Pref)
	}
}

func TestDNSResolver_CustomResolvers(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	cfg := &config.DNSConfig{
		Resolvers:      []string{"8.8.8.8:53", "1.1.1.1:53"}, // Google and Cloudflare DNS
		TimeoutSeconds: 5,
	}

	resolver := NewDNSResolver(cfg, logger)

	ctx := context.Background()
	records, err := resolver.LookupMX(ctx, "gmail.com")

	if err != nil {
		t.Fatalf("MX lookup with custom resolvers failed: %v", err)
	}

	if len(records) == 0 {
		t.Fatal("No MX records returned")
	}

	t.Logf("✓ Custom DNS resolvers working - found %d MX records", len(records))
}

func TestDNSResolver_Timeout(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	cfg := &config.DNSConfig{
		Resolvers:      []string{"192.0.2.1:53"}, // Non-routable IP (should timeout)
		TimeoutSeconds: 2,                        // Short timeout
	}

	resolver := NewDNSResolver(cfg, logger)

	ctx := context.Background()
	start := time.Now()
	_, err := resolver.LookupMX(ctx, "example.com")
	duration := time.Since(start)

	if err == nil {
		t.Fatal("Expected timeout error, got nil")
	}

	if duration > 3*time.Second {
		t.Errorf("Timeout took too long: %v (expected ~2s)", duration)
	}

	t.Logf("✓ Timeout working correctly (%v)", duration)
}

func TestDNSResolver_InvalidDomain(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	cfg := &config.DNSConfig{
		Resolvers:      []string{},
		TimeoutSeconds: 5,
	}

	resolver := NewDNSResolver(cfg, logger)

	ctx := context.Background()
	_, err := resolver.LookupMX(ctx, "this-domain-definitely-does-not-exist-12345.invalid")

	if err == nil {
		t.Fatal("Expected error for invalid domain, got nil")
	}

	t.Logf("✓ Invalid domain correctly rejected: %v", err)
}

func TestDNSResolver_HostLookup(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	cfg := &config.DNSConfig{
		Resolvers:      []string{},
		TimeoutSeconds: 5,
	}

	resolver := NewDNSResolver(cfg, logger)

	ctx := context.Background()
	addrs, err := resolver.LookupHost(ctx, "google.com")

	if err != nil {
		t.Fatalf("Host lookup failed: %v", err)
	}

	if len(addrs) == 0 {
		t.Fatal("No IP addresses returned")
	}

	t.Logf("✓ Found %d IP addresses for google.com", len(addrs))
	for _, addr := range addrs {
		t.Logf("  IP: %s", addr)
	}
}
