package delivery

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"strela/internal/config"

	"github.com/emersion/go-smtp"
)

// --- splitEmail ---

func TestSplitEmail(t *testing.T) {
	tests := []struct {
		email      string
		wantLocal  string
		wantDomain string
	}{
		{"user@example.com", "user", "example.com"},
		{"user+tag@sub.example.com", "user+tag", "sub.example.com"},
		{"@example.com", "", "example.com"},
		{"user@", "user", ""},
		{"noatsign", "", ""},
		{"", "", ""},
		{"a@b", "a", "b"},
		{"user@host@domain.com", "user@host", "domain.com"}, // LastIndex picks last @
	}

	for _, tt := range tests {
		t.Run(tt.email, func(t *testing.T) {
			local, domain := splitEmail(tt.email)
			if local != tt.wantLocal {
				t.Errorf("splitEmail(%q) local = %q, want %q", tt.email, local, tt.wantLocal)
			}
			if domain != tt.wantDomain {
				t.Errorf("splitEmail(%q) domain = %q, want %q", tt.email, domain, tt.wantDomain)
			}
		})
	}
}

// --- isBindError ---

func TestIsBindError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil error", err: nil, want: false},
		{name: "bind error", err: fmt.Errorf("dial tcp 0.0.0.0:0->1.2.3.4:25: bind: cannot assign requested address"), want: true},
		{name: "EADDRNOTAVAIL", err: fmt.Errorf("EADDRNOTAVAIL"), want: true},
		{name: "cannot assign requested address", err: fmt.Errorf("cannot assign requested address"), want: true},
		{name: "connection refused", err: fmt.Errorf("connection refused"), want: false},
		{name: "timeout", err: fmt.Errorf("i/o timeout"), want: false},
		{name: "EOF", err: fmt.Errorf("EOF"), want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isBindError(tt.err)
			if got != tt.want {
				t.Errorf("isBindError(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// --- IPRotator.SelectIPs ---

func TestIPRotator_SelectIPs_RoundRobin(t *testing.T) {
	ipsV4 := []string{"1.1.1.1", "2.2.2.2", "3.3.3.3"}
	r := NewIPRotator(ipsV4, nil, "round-robin")

	// First call should start at index 0
	result1 := r.SelectIPs(false, "example.com")
	if len(result1) != 3 {
		t.Fatalf("SelectIPs returned %d IPs, want 3", len(result1))
	}

	// Second call should start at index 1 (round-robin)
	result2 := r.SelectIPs(false, "example.com")
	if result2[0] == result1[0] {
		t.Error("Round-robin should rotate to next IP")
	}

	// After 3 calls, should cycle back
	_ = r.SelectIPs(false, "example.com")
	result4 := r.SelectIPs(false, "example.com")
	if result4[0] != result1[0] {
		t.Errorf("Expected cycle back to %s, got %s", result1[0], result4[0])
	}
}

func TestIPRotator_SelectIPs_HashDomain(t *testing.T) {
	ipsV4 := []string{"1.1.1.1", "2.2.2.2", "3.3.3.3"}
	r := NewIPRotator(ipsV4, nil, "hash-domain")

	// Same domain should always get the same start IP
	result1 := r.SelectIPs(false, "example.com")
	result2 := r.SelectIPs(false, "example.com")
	if result1[0] != result2[0] {
		t.Errorf("hash-domain should be deterministic: got %s then %s", result1[0], result2[0])
	}

	// Different domain may get a different start IP (not guaranteed but likely with 3 IPs)
	result3 := r.SelectIPs(false, "other-domain.org")
	// Just verify it returns all IPs
	if len(result3) != 3 {
		t.Fatalf("SelectIPs returned %d IPs, want 3", len(result3))
	}
}

func TestIPRotator_SelectIPs_Random(t *testing.T) {
	ipsV4 := []string{"1.1.1.1", "2.2.2.2", "3.3.3.3", "4.4.4.4", "5.5.5.5"}
	r := NewIPRotator(ipsV4, nil, "random")

	// Should return all IPs (rotated from random start)
	result := r.SelectIPs(false, "example.com")
	if len(result) != 5 {
		t.Fatalf("SelectIPs returned %d IPs, want 5", len(result))
	}

	// All original IPs should be present
	ipSet := make(map[string]bool)
	for _, ip := range result {
		ipSet[ip] = true
	}
	for _, ip := range ipsV4 {
		if !ipSet[ip] {
			t.Errorf("Missing IP %s in result", ip)
		}
	}
}

func TestIPRotator_SelectIPs_SingleIP(t *testing.T) {
	r := NewIPRotator([]string{"1.1.1.1"}, nil, "round-robin")
	result := r.SelectIPs(false, "example.com")
	if len(result) != 1 || result[0] != "1.1.1.1" {
		t.Errorf("Single IP should return [1.1.1.1], got %v", result)
	}
}

func TestIPRotator_SelectIPs_Empty(t *testing.T) {
	r := NewIPRotator(nil, nil, "round-robin")
	result := r.SelectIPs(false, "example.com")
	if result != nil {
		t.Errorf("Empty IP pool should return nil, got %v", result)
	}
}

func TestIPRotator_SelectIPs_IPv6(t *testing.T) {
	ipsV6 := []string{"2001:db8::1", "2001:db8::2"}
	r := NewIPRotator(nil, ipsV6, "round-robin")

	result := r.SelectIPs(true, "example.com")
	if len(result) != 2 {
		t.Fatalf("SelectIPs(ipv6) returned %d IPs, want 2", len(result))
	}

	// IPv4 should be empty
	v4Result := r.SelectIPs(false, "example.com")
	if v4Result != nil {
		t.Errorf("No IPv4 configured, expected nil, got %v", v4Result)
	}
}

func TestIPRotator_Properties(t *testing.T) {
	r := NewIPRotator([]string{"1.1.1.1"}, []string{"::1"}, "round-robin")

	if !r.HasIPv4() {
		t.Error("HasIPv4() should be true")
	}
	if !r.HasIPv6() {
		t.Error("HasIPv6() should be true")
	}
	if r.GetAllIPsV4()[0] != "1.1.1.1" {
		t.Error("GetAllIPsV4() wrong")
	}
	if r.GetAllIPsV6()[0] != "::1" {
		t.Error("GetAllIPsV6() wrong")
	}

	// Test RandomIntn
	for i := 0; i < 100; i++ {
		n := r.RandomIntn(10)
		if n < 0 || n >= 10 {
			t.Errorf("RandomIntn(10) = %d, out of range", n)
		}
	}
}

// --- shared test helpers ---

var defaultTestConfig = config.OutboundConfig{
	SMTPTimeoutSeconds:      60,
	MaxTotalDeliverySeconds: 200,
}

func testLogger() *slog.Logger {
	return slog.Default()
}

// --- mapSMTPError ---

func TestMapSMTPError(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	d := &Deliverer{logger: logger}

	tests := []struct {
		name       string
		err        error
		wantStatus string
	}{
		{
			name:       "550 permanent failure",
			err:        &smtp.SMTPError{Code: 550, Message: "User unknown"},
			wantStatus: "hard_bounce",
		},
		{
			name:       "553 permanent failure",
			err:        &smtp.SMTPError{Code: 553, Message: "Mailbox name not allowed"},
			wantStatus: "hard_bounce",
		},
		{
			name:       "421 temporary failure",
			err:        &smtp.SMTPError{Code: 421, Message: "Try again later"},
			wantStatus: "temp_fail",
		},
		{
			name:       "450 temporary failure",
			err:        &smtp.SMTPError{Code: 450, Message: "Mailbox busy"},
			wantStatus: "temp_fail",
		},
		{
			name:       "network error (non-SMTP) classified as timeout",
			err:        fmt.Errorf("connection reset by peer"),
			wantStatus: "timeout",
		},
		{
			name:       "bind error classified as error",
			err:        fmt.Errorf("bind: cannot assign requested address"),
			wantStatus: "error",
		},
		{
			name:       "EOF error classified as timeout",
			err:        fmt.Errorf("EOF"),
			wantStatus: "timeout",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := d.mapSMTPError(logger, "trace-123", tt.err, "mx.example.com", "1.2.3.4")
			if result.Status != tt.wantStatus {
				t.Errorf("mapSMTPError() status = %q, want %q (error: %v)", result.Status, tt.wantStatus, tt.err)
			}
			if result.TraceID != "trace-123" {
				t.Errorf("TraceID = %q, want %q", result.TraceID, "trace-123")
			}
			if result.MXHost != "mx.example.com" {
				t.Errorf("MXHost = %q, want %q", result.MXHost, "mx.example.com")
			}
		})
	}
}

func TestMapSMTPError_PreservesCodeAndMessage(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	d := &Deliverer{logger: logger}

	err := &smtp.SMTPError{Code: 550, Message: "5.1.1 User unknown"}
	result := d.mapSMTPError(logger, "t1", err, "mx.test.com", "1.2.3.4")

	if result.SMTPCode != 550 {
		t.Errorf("SMTPCode = %d, want 550", result.SMTPCode)
	}
	if result.SMTPMessage != "5.1.1 User unknown" {
		t.Errorf("SMTPMessage = %q, want %q", result.SMTPMessage, "5.1.1 User unknown")
	}
}

// --- waitForDomainRateLimit ---

func TestWaitForDomainRateLimit(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := &config.OutboundConfig{
		PerDomainIntervalSeconds: 1,
		PerDomainBurst:           2,
	}
	dnsCfg := &config.DNSConfig{}
	repCfg := &config.ReputationConfig{}
	expandedIPs := &config.ExpandedSourceIPs{}
	mxLookup := NewMXLookup(dnsCfg, logger)
	d := NewDeliverer(cfg, expandedIPs, mxLookup, logger, repCfg, nil, nil)

	ctx := context.Background()

	// First 2 requests should succeed (burst=2)
	if err := d.waitForDomainRateLimit(ctx, d.getConfig(), "test.com"); err != nil {
		t.Errorf("First request should succeed, got: %v", err)
	}
	if err := d.waitForDomainRateLimit(ctx, d.getConfig(), "test.com"); err != nil {
		t.Errorf("Second request (within burst) should succeed, got: %v", err)
	}

	// Third request should fail (burst exhausted, no time for refill)
	if err := d.waitForDomainRateLimit(ctx, d.getConfig(), "test.com"); err != ErrDomainRateLimitExceeded {
		t.Errorf("Third request should be rate-limited, got: %v", err)
	}

	// Different domain should have its own limiter
	if err := d.waitForDomainRateLimit(ctx, d.getConfig(), "other.com"); err != nil {
		t.Errorf("Different domain should not be rate-limited, got: %v", err)
	}
}

func TestWaitForDomainRateLimit_TokenRefill(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := &config.OutboundConfig{
		PerDomainIntervalSeconds: 1, // 1 token per second
		PerDomainBurst:           1,
	}
	dnsCfg := &config.DNSConfig{}
	repCfg := &config.ReputationConfig{}
	expandedIPs := &config.ExpandedSourceIPs{}
	mxLookup := NewMXLookup(dnsCfg, logger)
	d := NewDeliverer(cfg, expandedIPs, mxLookup, logger, repCfg, nil, nil)

	ctx := context.Background()

	// Consume the burst token
	if err := d.waitForDomainRateLimit(ctx, d.getConfig(), "refill-test.com"); err != nil {
		t.Fatalf("First request should succeed: %v", err)
	}

	// Should be rate-limited now
	if err := d.waitForDomainRateLimit(ctx, d.getConfig(), "refill-test.com"); err != ErrDomainRateLimitExceeded {
		t.Fatalf("Should be rate-limited after burst: %v", err)
	}

	// Wait for token refill
	time.Sleep(1100 * time.Millisecond)

	// Should succeed after refill
	if err := d.waitForDomainRateLimit(ctx, d.getConfig(), "refill-test.com"); err != nil {
		t.Errorf("Should succeed after token refill, got: %v", err)
	}
}

// --- cleanupDomainLimiters ---

func TestCleanupDomainLimiters(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := &config.OutboundConfig{
		PerDomainIntervalSeconds: 1,
		PerDomainBurst:           1,
	}
	dnsCfg := &config.DNSConfig{}
	repCfg := &config.ReputationConfig{}
	expandedIPs := &config.ExpandedSourceIPs{}
	mxLookup := NewMXLookup(dnsCfg, logger)
	d := NewDeliverer(cfg, expandedIPs, mxLookup, logger, repCfg, nil, nil)

	ctx := context.Background()

	// Create some domain limiters
	d.waitForDomainRateLimit(ctx, d.getConfig(), "domain1.com")
	d.waitForDomainRateLimit(ctx, d.getConfig(), "domain2.com")
	d.waitForDomainRateLimit(ctx, d.getConfig(), "domain3.com")

	// Verify they exist
	count := 0
	d.domainLimiters.Range(func(_, _ any) bool {
		count++
		return true
	})
	if count != 3 {
		t.Fatalf("Expected 3 domain limiters, got %d", count)
	}

	// Cleanup shouldn't remove them yet (they're recent)
	d.cleanupDomainLimiters()

	count = 0
	d.domainLimiters.Range(func(_, _ any) bool {
		count++
		return true
	})
	if count != 3 {
		t.Errorf("Recent limiters should not be cleaned up, got %d", count)
	}

	// Manually age one limiter to make it stale
	if limiterI, ok := d.domainLimiters.Load("domain1.com"); ok {
		limiter := limiterI.(*domainRateLimiter)
		limiter.mu.Lock()
		limiter.lastUpdate = time.Now().Add(-15 * time.Minute) // 15 min ago (> 10 min cutoff)
		limiter.mu.Unlock()
	}

	// Cleanup should remove the stale one
	d.cleanupDomainLimiters()

	count = 0
	d.domainLimiters.Range(func(_, _ any) bool {
		count++
		return true
	})
	if count != 2 {
		t.Errorf("Expected 2 domain limiters after cleanup, got %d", count)
	}

	// Verify the stale one was removed
	if _, ok := d.domainLimiters.Load("domain1.com"); ok {
		t.Error("Stale domain1.com limiter should have been removed")
	}
}

// --- WithTraceID / TraceIDFromContext ---

func TestTraceIDContext(t *testing.T) {
	ctx := context.Background()

	// Empty context should return empty string
	if id := TraceIDFromContext(ctx); id != "" {
		t.Errorf("Expected empty trace ID from background context, got %q", id)
	}

	// Set and retrieve
	ctx = WithTraceID(ctx, "abc123")
	if id := TraceIDFromContext(ctx); id != "abc123" {
		t.Errorf("Expected trace ID %q, got %q", "abc123", id)
	}

	// Child context inherits
	childCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	if id := TraceIDFromContext(childCtx); id != "abc123" {
		t.Errorf("Child context should inherit trace ID, got %q", id)
	}
}

// --- SetMetrics ---

func TestSetMetrics(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := &config.OutboundConfig{}
	dnsCfg := &config.DNSConfig{}
	repCfg := &config.ReputationConfig{}
	expandedIPs := &config.ExpandedSourceIPs{}
	mxLookup := NewMXLookup(dnsCfg, logger)
	d := NewDeliverer(cfg, expandedIPs, mxLookup, logger, repCfg, nil, nil)

	if d.metrics != nil {
		t.Error("Metrics should be nil initially")
	}

	mock := &mockMetrics{}
	d.SetMetrics(mock)

	if d.metrics != mock {
		t.Error("SetMetrics should set the metrics recorder")
	}

	// Test recordMetrics doesn't panic
	d.recordMetrics(DeliveryResult{Status: "delivered", AttemptDurationMs: 100}, "example.com")
	if mock.lastOutcome != "delivered" {
		t.Errorf("Expected outcome 'delivered', got %q", mock.lastOutcome)
	}
}

type mockMetrics struct {
	lastOutcome  string
	lastDuration float64
}

func (m *mockMetrics) RecordDeliveryAttempt(outcome, recipientDomain string, duration float64) {
	m.lastOutcome = outcome
	m.lastDuration = duration
}

// --- GetConnectionPool / GetReputationTracker ---

func TestGetConnectionPool(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := &config.OutboundConfig{ConnectionPoolTTLSeconds: 30}
	dnsCfg := &config.DNSConfig{}
	repCfg := &config.ReputationConfig{}
	expandedIPs := &config.ExpandedSourceIPs{}
	mxLookup := NewMXLookup(dnsCfg, logger)
	d := NewDeliverer(cfg, expandedIPs, mxLookup, logger, repCfg, nil, nil)

	pool := d.GetConnectionPool()
	if pool == nil {
		t.Error("GetConnectionPool() should not return nil")
	}
}

func TestGetReputationTracker(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := &config.OutboundConfig{}
	dnsCfg := &config.DNSConfig{}
	repCfg := &config.ReputationConfig{}
	expandedIPs := &config.ExpandedSourceIPs{}
	mxLookup := NewMXLookup(dnsCfg, logger)
	d := NewDeliverer(cfg, expandedIPs, mxLookup, logger, repCfg, nil, nil)

	tracker := d.GetReputationTracker()
	if tracker == nil {
		t.Error("GetReputationTracker() should not return nil")
	}
}

// --- Stop ---

func TestStop(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := &config.OutboundConfig{ConnectionPoolTTLSeconds: 30}
	dnsCfg := &config.DNSConfig{}
	repCfg := &config.ReputationConfig{}
	expandedIPs := &config.ExpandedSourceIPs{}
	mxLookup := NewMXLookup(dnsCfg, logger)
	d := NewDeliverer(cfg, expandedIPs, mxLookup, logger, repCfg, nil, nil)

	// Stop should not panic
	d.Stop()

	// Double stop should not panic either
	// (stopCh is already closed, but that's handled by deferred recover in SafeGo)
}
