package delivery

import (
	"context"
	"fmt"
	"testing"
	"time"

	"fune/internal/config"
	"fune/internal/queue"

	"github.com/emersion/go-smtp"
	"go.uber.org/zap"
)

func TestIPRotator_RoundRobin(t *testing.T) {
	ips := []string{"192.168.1.1", "192.168.1.2", "192.168.1.3"}
	rotator := NewIPRotator(ips, "round-robin")

	// Should cycle through IPs in order
	expected := []string{
		"192.168.1.1",
		"192.168.1.2",
		"192.168.1.3",
		"192.168.1.1", // Back to first
		"192.168.1.2",
	}

	for i, expectedIP := range expected {
		ip := rotator.SelectIP("example.com")
		if ip != expectedIP {
			t.Errorf("Round %d: expected %s, got %s", i+1, expectedIP, ip)
		}
	}
}

func TestIPRotator_Random(t *testing.T) {
	ips := []string{"192.168.1.1", "192.168.1.2", "192.168.1.3"}
	rotator := NewIPRotator(ips, "random")

	// Get several IPs and verify they're from the pool
	seen := make(map[string]bool)
	for i := 0; i < 20; i++ {
		ip := rotator.SelectIP("example.com")

		// Verify IP is in pool
		found := false
		for _, validIP := range ips {
			if ip == validIP {
				found = true
				break
			}
		}

		if !found {
			t.Errorf("Random selection returned invalid IP: %s", ip)
		}

		seen[ip] = true
	}

	// Should have seen multiple different IPs (probabilistic test)
	if len(seen) < 2 {
		t.Error("Random selection should use multiple IPs")
	}
}

func TestIPRotator_HashDomain(t *testing.T) {
	ips := []string{"192.168.1.1", "192.168.1.2", "192.168.1.3"}
	rotator := NewIPRotator(ips, "hash-domain")

	// Same domain should always get same IP
	domain := "example.com"
	ip1 := rotator.SelectIP(domain)
	ip2 := rotator.SelectIP(domain)
	ip3 := rotator.SelectIP(domain)

	if ip1 != ip2 || ip2 != ip3 {
		t.Errorf("Same domain should get same IP: %s, %s, %s", ip1, ip2, ip3)
	}

	// Different domains should potentially get different IPs
	domainA := rotator.SelectIP("domainA.com")
	domainB := rotator.SelectIP("domainB.com")
	domainC := rotator.SelectIP("domainC.com")

	// All should be valid IPs
	for _, ip := range []string{domainA, domainB, domainC} {
		found := false
		for _, validIP := range ips {
			if ip == validIP {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("Hash domain returned invalid IP: %s", ip)
		}
	}
}

func TestIPRotator_SingleIP(t *testing.T) {
	ips := []string{"192.168.1.1"}
	rotator := NewIPRotator(ips, "round-robin")

	// Should always return the single IP
	for i := 0; i < 5; i++ {
		ip := rotator.SelectIP("example.com")
		if ip != "192.168.1.1" {
			t.Errorf("Expected single IP 192.168.1.1, got %s", ip)
		}
	}
}

func TestIPRotator_NoIPs(t *testing.T) {
	ips := []string{}
	rotator := NewIPRotator(ips, "round-robin")

	// Should return empty string
	ip := rotator.SelectIP("example.com")
	if ip != "" {
		t.Errorf("Expected empty string for no IPs, got %s", ip)
	}
}

func TestIPRotator_InvalidStrategy(t *testing.T) {
	ips := []string{"192.168.1.1", "192.168.1.2"}
	rotator := NewIPRotator(ips, "invalid-strategy")

	// Should fall back to round-robin
	ip1 := rotator.SelectIP("example.com")
	ip2 := rotator.SelectIP("example.com")

	if ip1 != "192.168.1.1" || ip2 != "192.168.1.2" {
		t.Errorf("Invalid strategy should fall back to round-robin")
	}
}

func TestExtractSMTPError(t *testing.T) {
	// Test with SMTP error
	smtpErr := &smtp.SMTPError{
		Code:    550,
		Message: "User not found",
	}

	code, resp := extractSMTPError(smtpErr)
	if code != 550 {
		t.Errorf("Expected code 550, got %d", code)
	}
	if resp != "User not found" {
		t.Errorf("Expected response 'User not found', got '%s'", resp)
	}
}

func TestExtractSMTPError_GenericError(t *testing.T) {
	// Test with generic error
	err := fmt.Errorf("connection refused")

	code, resp := extractSMTPError(err)
	if code != 0 {
		t.Errorf("Expected code 0 for generic error, got %d", code)
	}
	if resp != "connection refused" {
		t.Errorf("Expected error message, got '%s'", resp)
	}
}

func TestDeliverer_DeliverMessage_NoMXRecords(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	q, cleanup := queue.SetupTestQueue(t)
	defer cleanup()

	cfg := &config.OutboundConfig{
		SourceIPs:                []string{"127.0.0.1"},
		SourceIPSelection:        "round-robin",
		MXCacheTTLSeconds:        3600,
		ConnectionTimeoutSeconds: 5,
		SMTPTimeoutSeconds:       10,
	}

	reputationCfg := &config.ReputationConfig{EnableIPTracking: false}
	dnsCfg := &config.DNSConfig{}
	mxLookup := NewMXLookup(q, dnsCfg, cfg, logger)
	deliverer := NewDeliverer(cfg, mxLookup, logger, reputationCfg)

	msg := &queue.QueuedMessage{
		MessageID:  "test_msg",
		FromAddr:   "sender@example.com",
		ToAddr:     "user@invalid-domain-does-not-exist-12345.com",
		ToDomain:   "invalid-domain-does-not-exist-12345.com",
		Subject:    "Test",
		RawMessage: []byte("From: sender@example.com\r\n\r\nTest"),
		Attempts:   0,
	}

	ctx := context.Background()
	result := deliverer.DeliverMessage(ctx, msg)

	if result.Success {
		t.Error("Expected delivery to fail for domain with no MX records")
	}

	if result.Error == nil {
		t.Error("Expected error for failed delivery")
	}

	if result.Error.Category != ErrorNetwork {
		t.Errorf("Expected network error category, got %s", result.Error.Category)
	}
}

func TestDeliverer_SelectSourceIP(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	q, cleanup := queue.SetupTestQueue(t)
	defer cleanup()

	cfg := &config.OutboundConfig{
		SourceIPs:                []string{"192.168.1.1", "192.168.1.2"},
		SourceIPSelection:        "round-robin",
		MXCacheTTLSeconds:        3600,
		ConnectionTimeoutSeconds: 5,
		SMTPTimeoutSeconds:       10,
	}

	reputationCfg := &config.ReputationConfig{EnableIPTracking: false}
	dnsCfg := &config.DNSConfig{}
	mxLookup := NewMXLookup(q, dnsCfg, cfg, logger)
	deliverer := NewDeliverer(cfg, mxLookup, logger, reputationCfg)

	// Verify IP rotator is initialized
	if deliverer.ipRotator == nil {
		t.Error("IP rotator should be initialized")
	}

	// Test IP selection
	ip1 := deliverer.ipRotator.SelectIP("example.com")
	ip2 := deliverer.ipRotator.SelectIP("example.com")

	if ip1 != "192.168.1.1" {
		t.Errorf("First IP should be 192.168.1.1, got %s", ip1)
	}
	if ip2 != "192.168.1.2" {
		t.Errorf("Second IP should be 192.168.1.2, got %s", ip2)
	}
}

func TestDeliveryResult_Success(t *testing.T) {
	result := &DeliveryResult{
		Success:  true,
		SMTPCode: 250,
	}

	if !result.Success {
		t.Error("Expected successful result")
	}

	if result.SMTPCode != 250 {
		t.Errorf("Expected SMTP code 250, got %d", result.SMTPCode)
	}

	if result.Error != nil {
		t.Error("Successful delivery should have no error")
	}
}

func TestDeliveryResult_Failure(t *testing.T) {
	result := &DeliveryResult{
		Success: false,
		Error: &DeliveryError{
			Category:     ErrorPermanent,
			SMTPCode:     550,
			SMTPResponse: "User not found",
			Message:      "User not found",
		},
	}

	if result.Success {
		t.Error("Expected failed result")
	}

	if result.Error == nil {
		t.Error("Failed delivery should have error")
	}

	if result.Error.Category != ErrorPermanent {
		t.Errorf("Expected permanent error, got %s", result.Error.Category)
	}
}

func TestIPRotator_HashDomain_Consistency(t *testing.T) {
	ips := []string{"192.168.1.1", "192.168.1.2", "192.168.1.3"}

	// Create two rotators with same config
	rotator1 := NewIPRotator(ips, "hash-domain")
	rotator2 := NewIPRotator(ips, "hash-domain")

	domains := []string{"example.com", "test.com", "foo.com", "bar.com"}

	for _, domain := range domains {
		ip1 := rotator1.SelectIP(domain)
		ip2 := rotator2.SelectIP(domain)

		if ip1 != ip2 {
			t.Errorf("Domain %s: different rotators should give same IP with hash strategy: %s vs %s",
				domain, ip1, ip2)
		}
	}
}

func TestIPRotator_RoundRobin_ThreadSafety(t *testing.T) {
	// Note: This is a basic concurrency test
	// In production, IPRotator.counter should use atomic operations
	ips := []string{"192.168.1.1", "192.168.1.2", "192.168.1.3"}
	rotator := NewIPRotator(ips, "round-robin")

	// Multiple goroutines selecting IPs
	done := make(chan bool)
	for i := 0; i < 10; i++ {
		go func() {
			for j := 0; j < 100; j++ {
				ip := rotator.SelectIP("example.com")
				// Verify it's a valid IP
				found := false
				for _, validIP := range ips {
					if ip == validIP {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("Invalid IP selected: %s", ip)
				}
			}
			done <- true
		}()
	}

	// Wait for all goroutines
	for i := 0; i < 10; i++ {
		<-done
	}
}

func TestDeliverer_LoggingOnFailure(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	q, cleanup := queue.SetupTestQueue(t)
	defer cleanup()

	cfg := &config.OutboundConfig{
		SourceIPs:                []string{},
		SourceIPSelection:        "round-robin",
		MXCacheTTLSeconds:        3600,
		ConnectionTimeoutSeconds: 5,
		SMTPTimeoutSeconds:       10,
	}

	reputationCfg := &config.ReputationConfig{EnableIPTracking: false}
	dnsCfg := &config.DNSConfig{}
	mxLookup := NewMXLookup(q, dnsCfg, cfg, logger)
	deliverer := NewDeliverer(cfg, mxLookup, logger, reputationCfg)

	msg := &queue.QueuedMessage{
		MessageID:  "test_msg",
		FromAddr:   "sender@example.com",
		ToAddr:     "user@nonexistent-domain-12345.com",
		ToDomain:   "nonexistent-domain-12345.com",
		Subject:    "Test",
		RawMessage: []byte("From: sender@example.com\r\n\r\nTest"),
		Attempts:   0,
	}

	// Should not panic and should return error
	ctx := context.Background()
	result := deliverer.DeliverMessage(ctx, msg)

	if result == nil {
		t.Fatal("Expected delivery result, got nil")
	}

	if result.Success {
		t.Error("Expected delivery to fail")
	}
}

func TestDeliverer_EmptySourceIPs(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	q, cleanup := queue.SetupTestQueue(t)
	defer cleanup()

	cfg := &config.OutboundConfig{
		SourceIPs:                []string{}, // No source IPs
		SourceIPSelection:        "round-robin",
		MXCacheTTLSeconds:        3600,
		ConnectionTimeoutSeconds: 5,
		SMTPTimeoutSeconds:       10,
	}

	reputationCfg := &config.ReputationConfig{EnableIPTracking: false}
	dnsCfg := &config.DNSConfig{}
	mxLookup := NewMXLookup(q, dnsCfg, cfg, logger)
	deliverer := NewDeliverer(cfg, mxLookup, logger, reputationCfg)

	// Should handle empty source IPs gracefully
	if deliverer.ipRotator == nil {
		t.Error("IP rotator should be initialized even with empty IPs")
	}

	ip := deliverer.ipRotator.SelectIP("example.com")
	if ip != "" {
		t.Errorf("Expected empty IP for no source IPs, got %s", ip)
	}
}

func TestDeliveryResult_Duration(t *testing.T) {
	start := time.Now()

	// Simulate some work
	time.Sleep(10 * time.Millisecond)

	duration := time.Since(start).Milliseconds()

	result := &DeliveryResult{
		DurationMs: duration,
	}

	if result.DurationMs < 10 {
		t.Errorf("Expected duration >= 10ms, got %dms", result.DurationMs)
	}
}

func TestIPRotator_Distribution(t *testing.T) {
	ips := []string{"192.168.1.1", "192.168.1.2", "192.168.1.3"}
	rotator := NewIPRotator(ips, "random")

	// Get 300 samples
	counts := make(map[string]int)
	for i := 0; i < 300; i++ {
		ip := rotator.SelectIP("example.com")
		counts[ip]++
	}

	// Each IP should get roughly 100 selections (with some variance)
	for ip, count := range counts {
		if count < 50 || count > 150 {
			t.Errorf("IP %s: expected ~100 selections, got %d (poor distribution)", ip, count)
		}
	}

	// Should have seen all 3 IPs
	if len(counts) != 3 {
		t.Errorf("Expected all 3 IPs to be selected, only saw %d", len(counts))
	}
}

func TestDeliverer_ReloadConfig(t *testing.T) {
	logger := zap.NewNop()

	// Initial config
	initialCfg := &config.OutboundConfig{
		SourceIPs:                      []string{"192.168.1.100"},
		SourceIPSelection:              "round-robin",
		PerDomainIntervalSeconds:       2,
		CircuitBreakerEnabled:          true,
		CircuitBreakerFailureThreshold: 5,
		CircuitBreakerSuccessThreshold: 2,
		CircuitBreakerOpenTimeoutSecs:  60,
	}

	reputationCfg := &config.ReputationConfig{EnableIPTracking: false}
	mxLookup := &MXLookup{logger: logger}
	deliverer := NewDeliverer(initialCfg, mxLookup, logger, reputationCfg)

	// Verify initial config
	if len(deliverer.ipRotator.GetAllIPs()) != 1 {
		t.Errorf("expected 1 source IP, got %d", len(deliverer.ipRotator.GetAllIPs()))
	}
	if deliverer.circuitBreaker == nil {
		t.Error("circuit breaker should be enabled")
	}

	// New config with different settings
	newCfg := &config.OutboundConfig{
		SourceIPs:                      []string{"192.168.1.100", "192.168.1.101", "192.168.1.102"},
		SourceIPSelection:              "random",
		PerDomainIntervalSeconds:       5,
		CircuitBreakerEnabled:          true,
		CircuitBreakerFailureThreshold: 10,
		CircuitBreakerSuccessThreshold: 3,
		CircuitBreakerOpenTimeoutSecs:  120,
	}

	// Reload config
	if err := deliverer.ReloadConfig(newCfg); err != nil {
		t.Fatalf("ReloadConfig failed: %v", err)
	}

	// Verify updated config
	if len(deliverer.ipRotator.GetAllIPs()) != 3 {
		t.Errorf("expected 3 source IPs after reload, got %d", len(deliverer.ipRotator.GetAllIPs()))
	}
	if deliverer.config.SourceIPSelection != "random" {
		t.Errorf("expected source_ip_selection 'random', got '%s'", deliverer.config.SourceIPSelection)
	}
	if deliverer.circuitBreaker == nil {
		t.Error("circuit breaker should still be enabled")
	}

	// Verify circuit breaker thresholds updated
	deliverer.circuitBreaker.mu.RLock()
	if deliverer.circuitBreaker.failureThreshold != 10 {
		t.Errorf("expected failure threshold 10, got %d", deliverer.circuitBreaker.failureThreshold)
	}
	if deliverer.circuitBreaker.successThreshold != 3 {
		t.Errorf("expected success threshold 3, got %d", deliverer.circuitBreaker.successThreshold)
	}
	deliverer.circuitBreaker.mu.RUnlock()
}

func TestDeliverer_ReloadConfig_DisableCircuitBreaker(t *testing.T) {
	logger := zap.NewNop()

	// Initial config with circuit breaker enabled
	initialCfg := &config.OutboundConfig{
		SourceIPs:                      []string{"192.168.1.100"},
		SourceIPSelection:              "round-robin",
		CircuitBreakerEnabled:          true,
		CircuitBreakerFailureThreshold: 5,
		CircuitBreakerSuccessThreshold: 2,
		CircuitBreakerOpenTimeoutSecs:  60,
	}

	reputationCfg := &config.ReputationConfig{EnableIPTracking: false}
	mxLookup := &MXLookup{logger: logger}
	deliverer := NewDeliverer(initialCfg, mxLookup, logger, reputationCfg)

	if deliverer.circuitBreaker == nil {
		t.Error("circuit breaker should be enabled initially")
	}

	// New config with circuit breaker disabled
	newCfg := &config.OutboundConfig{
		SourceIPs:             []string{"192.168.1.100"},
		SourceIPSelection:     "round-robin",
		CircuitBreakerEnabled: false,
	}

	// Reload config
	if err := deliverer.ReloadConfig(newCfg); err != nil {
		t.Fatalf("ReloadConfig failed: %v", err)
	}

	// Verify circuit breaker is now disabled
	if deliverer.circuitBreaker != nil {
		t.Error("circuit breaker should be disabled after reload")
	}
}

func TestDeliverer_ReloadConfig_EnableCircuitBreaker(t *testing.T) {
	logger := zap.NewNop()

	// Initial config with circuit breaker disabled
	initialCfg := &config.OutboundConfig{
		SourceIPs:             []string{"192.168.1.100"},
		SourceIPSelection:     "round-robin",
		CircuitBreakerEnabled: false,
	}

	reputationCfg := &config.ReputationConfig{EnableIPTracking: false}
	mxLookup := &MXLookup{logger: logger}
	deliverer := NewDeliverer(initialCfg, mxLookup, logger, reputationCfg)

	if deliverer.circuitBreaker != nil {
		t.Error("circuit breaker should be disabled initially")
	}

	// New config with circuit breaker enabled
	newCfg := &config.OutboundConfig{
		SourceIPs:                      []string{"192.168.1.100"},
		SourceIPSelection:              "round-robin",
		CircuitBreakerEnabled:          true,
		CircuitBreakerFailureThreshold: 5,
		CircuitBreakerSuccessThreshold: 2,
		CircuitBreakerOpenTimeoutSecs:  60,
	}

	// Reload config
	if err := deliverer.ReloadConfig(newCfg); err != nil {
		t.Fatalf("ReloadConfig failed: %v", err)
	}

	// Verify circuit breaker is now enabled
	if deliverer.circuitBreaker == nil {
		t.Error("circuit breaker should be enabled after reload")
	}
}
