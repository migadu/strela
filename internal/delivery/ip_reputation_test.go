package delivery

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"fune/internal/config"
	"fune/internal/queue"

	"go.uber.org/zap"
)

func setupTestReputationTracker(_ *testing.T, cfg *config.ReputationConfig) *IPReputationTracker {
	logger := zap.NewNop()
	if cfg == nil {
		cfg = &config.ReputationConfig{
			EnableIPTracking:       true,
			DegradedRetryHours:     48,
			AlertTimeoutSeconds:    10,
			DegradedIPCleanupHours: 168,
		}
	}
	return NewIPReputationTracker(cfg, logger)
}

func TestIPReputationTracker_Creation(t *testing.T) {
	tracker := setupTestReputationTracker(t, nil)

	if tracker == nil {
		t.Fatal("tracker should not be nil")
	}

	if !tracker.enabled {
		t.Error("tracker should be enabled by default")
	}

	if tracker.degradedIPs == nil {
		t.Error("degradedIPs map should be initialized")
	}

	if len(tracker.degradedIPs) != 0 {
		t.Errorf("degradedIPs should be empty initially, got %d", len(tracker.degradedIPs))
	}
}

func TestIPReputationTracker_Disabled(t *testing.T) {
	cfg := &config.ReputationConfig{
		EnableIPTracking:       false,
		DegradedRetryHours:     48,
		AlertTimeoutSeconds:    10,
		DegradedIPCleanupHours: 168,
	}
	tracker := setupTestReputationTracker(t, cfg)

	if tracker.enabled {
		t.Error("tracker should be disabled")
	}

	// When disabled, all IPs should be considered healthy
	if !tracker.IsIPHealthy("192.168.1.100") {
		t.Error("IP should be healthy when tracking is disabled")
	}

	// Mark IP as degraded (should be no-op)
	deliveryInfo := DeliveryInfo{
		From:    "sender@example.com",
		To:      "recipient@example.com",
		Subject: "Test",
		MXHost:  "mx.example.com",
	}
	tracker.MarkIPDegraded("192.168.1.100", 550, "Blocked", deliveryInfo)

	// Should still be healthy
	if !tracker.IsIPHealthy("192.168.1.100") {
		t.Error("IP should remain healthy when tracking is disabled")
	}

	if len(tracker.degradedIPs) != 0 {
		t.Errorf("degradedIPs should remain empty when disabled, got %d", len(tracker.degradedIPs))
	}
}

func TestIPReputationTracker_IsIPHealthy(t *testing.T) {
	tracker := setupTestReputationTracker(t, nil)

	// Non-tracked IP should be healthy
	if !tracker.IsIPHealthy("192.168.1.100") {
		t.Error("non-tracked IP should be healthy")
	}

	// Mark IP as degraded
	deliveryInfo := DeliveryInfo{
		From:    "sender@example.com",
		To:      "recipient@example.com",
		Subject: "Test",
		MXHost:  "mx.example.com",
	}
	tracker.MarkIPDegraded("192.168.1.100", 550, "Blocked by Spamhaus", deliveryInfo)

	// Should now be degraded
	if tracker.IsIPHealthy("192.168.1.100") {
		t.Error("degraded IP should not be healthy")
	}

	// Different IP should still be healthy
	if !tracker.IsIPHealthy("192.168.1.101") {
		t.Error("different IP should be healthy")
	}
}

func TestIPReputationTracker_MarkIPDegraded(t *testing.T) {
	tracker := setupTestReputationTracker(t, nil)

	deliveryInfo := DeliveryInfo{
		From:           "sender@example.com",
		To:             "recipient@example.com",
		Subject:        "Test Subject",
		IdempotencyKey: "test-key-123",
		MXHost:         "mx.example.com",
	}

	tracker.MarkIPDegraded("192.168.1.100", 550, "IP blocked by Spamhaus", deliveryInfo)

	// Verify IP is in degraded map
	degradedIPs := tracker.GetDegradedIPs()
	if len(degradedIPs) != 1 {
		t.Fatalf("expected 1 degraded IP, got %d", len(degradedIPs))
	}

	info, exists := degradedIPs["192.168.1.100"]
	if !exists {
		t.Fatal("IP should be in degraded map")
	}

	if info.IP != "192.168.1.100" {
		t.Errorf("expected IP 192.168.1.100, got %s", info.IP)
	}

	if info.LastSMTPCode != 550 {
		t.Errorf("expected SMTP code 550, got %d", info.LastSMTPCode)
	}

	if info.LastSMTPResponse != "IP blocked by Spamhaus" {
		t.Errorf("expected response 'IP blocked by Spamhaus', got %s", info.LastSMTPResponse)
	}

	if info.FailureCount != 1 {
		t.Errorf("expected failure count 1, got %d", info.FailureCount)
	}

	// Verify retry time is set correctly (48 hours from now)
	expectedRetry := time.Now().Add(48 * time.Hour)
	if info.RetryAfter.Before(expectedRetry.Add(-1*time.Minute)) || info.RetryAfter.After(expectedRetry.Add(1*time.Minute)) {
		t.Errorf("retry time not set correctly, got %v, expected around %v", info.RetryAfter, expectedRetry)
	}
}

func TestIPReputationTracker_MarkIPDegraded_Multiple(t *testing.T) {
	tracker := setupTestReputationTracker(t, nil)

	deliveryInfo := DeliveryInfo{
		From:    "sender@example.com",
		To:      "recipient@example.com",
		Subject: "Test",
		MXHost:  "mx.example.com",
	}

	// Mark same IP degraded multiple times
	tracker.MarkIPDegraded("192.168.1.100", 550, "First failure", deliveryInfo)
	time.Sleep(10 * time.Millisecond)
	tracker.MarkIPDegraded("192.168.1.100", 554, "Second failure", deliveryInfo)
	time.Sleep(10 * time.Millisecond)
	tracker.MarkIPDegraded("192.168.1.100", 550, "Third failure", deliveryInfo)

	degradedIPs := tracker.GetDegradedIPs()
	if len(degradedIPs) != 1 {
		t.Fatalf("expected 1 degraded IP, got %d", len(degradedIPs))
	}

	info := degradedIPs["192.168.1.100"]
	if info.FailureCount != 3 {
		t.Errorf("expected failure count 3, got %d", info.FailureCount)
	}

	// Should have most recent error details
	if info.LastSMTPCode != 550 {
		t.Errorf("expected last SMTP code 550, got %d", info.LastSMTPCode)
	}

	if info.LastSMTPResponse != "Third failure" {
		t.Errorf("expected last response 'Third failure', got %s", info.LastSMTPResponse)
	}
}

func TestIPReputationTracker_MarkIPRecovered(t *testing.T) {
	tracker := setupTestReputationTracker(t, nil)

	deliveryInfo := DeliveryInfo{
		From:    "sender@example.com",
		To:      "recipient@example.com",
		Subject: "Test",
		MXHost:  "mx.example.com",
	}

	// Mark IP as degraded
	tracker.MarkIPDegraded("192.168.1.100", 550, "Blocked", deliveryInfo)

	if tracker.IsIPHealthy("192.168.1.100") {
		t.Error("IP should be degraded")
	}

	// Mark as recovered
	tracker.MarkIPRecovered("192.168.1.100")

	// Should now be healthy
	if !tracker.IsIPHealthy("192.168.1.100") {
		t.Error("recovered IP should be healthy")
	}

	// Should be removed from degraded map
	degradedIPs := tracker.GetDegradedIPs()
	if len(degradedIPs) != 0 {
		t.Errorf("expected 0 degraded IPs, got %d", len(degradedIPs))
	}
}

func TestIPReputationTracker_RecordDeliveryAttempt_Success(t *testing.T) {
	tracker := setupTestReputationTracker(t, nil)

	deliveryInfo := DeliveryInfo{
		From:    "sender@example.com",
		To:      "recipient@example.com",
		Subject: "Test",
		MXHost:  "mx.example.com",
	}

	// Mark IP as degraded
	tracker.MarkIPDegraded("192.168.1.100", 550, "Blocked", deliveryInfo)

	if tracker.IsIPHealthy("192.168.1.100") {
		t.Error("IP should be degraded")
	}

	// Record successful delivery (should recover the IP)
	tracker.RecordDeliveryAttempt("192.168.1.100", true, nil, deliveryInfo)

	// Should now be healthy
	if !tracker.IsIPHealthy("192.168.1.100") {
		t.Error("IP should be recovered after successful delivery")
	}

	degradedIPs := tracker.GetDegradedIPs()
	if len(degradedIPs) != 0 {
		t.Errorf("expected 0 degraded IPs after recovery, got %d", len(degradedIPs))
	}
}

func TestIPReputationTracker_RecordDeliveryAttempt_ReputationFailure(t *testing.T) {
	tracker := setupTestReputationTracker(t, nil)

	deliveryInfo := DeliveryInfo{
		From:    "sender@example.com",
		To:      "recipient@example.com",
		Subject: "Test",
		MXHost:  "mx.example.com",
	}

	// Record reputation failure
	err := &DeliveryError{
		Category:     ErrorReputation,
		SMTPCode:     550,
		SMTPResponse: "Blocked by RBL",
		Message:      "IP reputation error",
	}

	tracker.RecordDeliveryAttempt("192.168.1.100", false, err, deliveryInfo)

	// IP should be degraded
	if tracker.IsIPHealthy("192.168.1.100") {
		t.Error("IP should be degraded after reputation failure")
	}

	degradedIPs := tracker.GetDegradedIPs()
	if len(degradedIPs) != 1 {
		t.Fatalf("expected 1 degraded IP, got %d", len(degradedIPs))
	}

	info := degradedIPs["192.168.1.100"]
	if info.LastSMTPCode != 550 {
		t.Errorf("expected SMTP code 550, got %d", info.LastSMTPCode)
	}
}

func TestIPReputationTracker_RecordDeliveryAttempt_NonReputationFailure(t *testing.T) {
	tracker := setupTestReputationTracker(t, nil)

	deliveryInfo := DeliveryInfo{
		From:    "sender@example.com",
		To:      "recipient@example.com",
		Subject: "Test",
		MXHost:  "mx.example.com",
	}

	// Record non-reputation failure (temporary error)
	err := &DeliveryError{
		Category:     ErrorTemporary,
		SMTPCode:     421,
		SMTPResponse: "Try again later",
		Message:      "Temporary error",
	}

	tracker.RecordDeliveryAttempt("192.168.1.100", false, err, deliveryInfo)

	// IP should still be healthy (not a reputation issue)
	if !tracker.IsIPHealthy("192.168.1.100") {
		t.Error("IP should remain healthy for non-reputation failures")
	}

	degradedIPs := tracker.GetDegradedIPs()
	if len(degradedIPs) != 0 {
		t.Errorf("expected 0 degraded IPs for non-reputation failure, got %d", len(degradedIPs))
	}
}

func TestIPReputationTracker_GetHealthyIPs(t *testing.T) {
	tracker := setupTestReputationTracker(t, nil)

	deliveryInfo := DeliveryInfo{
		From:    "sender@example.com",
		To:      "recipient@example.com",
		Subject: "Test",
		MXHost:  "mx.example.com",
	}

	allIPs := []string{"192.168.1.100", "192.168.1.101", "192.168.1.102", "192.168.1.103"}

	// Initially all IPs should be healthy
	healthy := tracker.GetHealthyIPs(allIPs)
	if len(healthy) != 4 {
		t.Errorf("expected 4 healthy IPs, got %d", len(healthy))
	}

	// Degrade two IPs
	tracker.MarkIPDegraded("192.168.1.100", 550, "Blocked", deliveryInfo)
	tracker.MarkIPDegraded("192.168.1.102", 550, "Blocked", deliveryInfo)

	// Should return only healthy IPs
	healthy = tracker.GetHealthyIPs(allIPs)
	if len(healthy) != 2 {
		t.Errorf("expected 2 healthy IPs, got %d", len(healthy))
	}

	// Verify correct IPs are returned
	healthyMap := make(map[string]bool)
	for _, ip := range healthy {
		healthyMap[ip] = true
	}

	if !healthyMap["192.168.1.101"] {
		t.Error("192.168.1.101 should be in healthy list")
	}
	if !healthyMap["192.168.1.103"] {
		t.Error("192.168.1.103 should be in healthy list")
	}
	if healthyMap["192.168.1.100"] {
		t.Error("192.168.1.100 should not be in healthy list")
	}
	if healthyMap["192.168.1.102"] {
		t.Error("192.168.1.102 should not be in healthy list")
	}
}

func TestIPReputationTracker_RetryTimeExpired(t *testing.T) {
	// Use shorter retry time for testing
	cfg := &config.ReputationConfig{
		EnableIPTracking:       true,
		DegradedRetryHours:     0, // Set to 0 so retry time is immediate
		AlertTimeoutSeconds:    10,
		DegradedIPCleanupHours: 168,
	}
	tracker := setupTestReputationTracker(t, cfg)

	deliveryInfo := DeliveryInfo{
		From:    "sender@example.com",
		To:      "recipient@example.com",
		Subject: "Test",
		MXHost:  "mx.example.com",
	}

	// Mark IP as degraded
	tracker.MarkIPDegraded("192.168.1.100", 550, "Blocked", deliveryInfo)

	// With 0 retry hours, retry time should be immediate
	// Small sleep to ensure time has passed
	time.Sleep(10 * time.Millisecond)

	// Should be considered healthy now (retry time passed)
	if !tracker.IsIPHealthy("192.168.1.100") {
		t.Error("IP should be healthy after retry time expires")
	}

	// But should still be in degraded map until recovered
	degradedIPs := tracker.GetDegradedIPs()
	if len(degradedIPs) != 1 {
		t.Errorf("expected 1 degraded IP in map, got %d", len(degradedIPs))
	}
}

func TestIPReputationTracker_Cleanup(t *testing.T) {
	// Use short cleanup time for testing
	cfg := &config.ReputationConfig{
		EnableIPTracking:       true,
		DegradedRetryHours:     0, // Immediate retry
		AlertTimeoutSeconds:    10,
		DegradedIPCleanupHours: 0, // Immediate cleanup eligible
	}
	tracker := setupTestReputationTracker(t, cfg)

	deliveryInfo := DeliveryInfo{
		From:    "sender@example.com",
		To:      "recipient@example.com",
		Subject: "Test",
		MXHost:  "mx.example.com",
	}

	// Mark IPs as degraded
	tracker.MarkIPDegraded("192.168.1.100", 550, "Blocked", deliveryInfo)
	tracker.MarkIPDegraded("192.168.1.101", 550, "Blocked", deliveryInfo)
	tracker.MarkIPDegraded("192.168.1.102", 550, "Blocked", deliveryInfo)

	if len(tracker.degradedIPs) != 3 {
		t.Fatalf("expected 3 degraded IPs, got %d", len(tracker.degradedIPs))
	}

	// Wait for retry time to pass
	time.Sleep(10 * time.Millisecond)

	// Run cleanup
	tracker.Cleanup()

	// All entries should be cleaned up (degraded time is old and retry time passed)
	if len(tracker.degradedIPs) != 0 {
		t.Errorf("expected 0 degraded IPs after cleanup, got %d", len(tracker.degradedIPs))
	}
}

func TestIPReputationTracker_Cleanup_PreservesRecent(t *testing.T) {
	cfg := &config.ReputationConfig{
		EnableIPTracking:       true,
		DegradedRetryHours:     48,
		AlertTimeoutSeconds:    10,
		DegradedIPCleanupHours: 168, // 7 days
	}
	tracker := setupTestReputationTracker(t, cfg)

	deliveryInfo := DeliveryInfo{
		From:    "sender@example.com",
		To:      "recipient@example.com",
		Subject: "Test",
		MXHost:  "mx.example.com",
	}

	// Mark IP as degraded (recent)
	tracker.MarkIPDegraded("192.168.1.100", 550, "Blocked", deliveryInfo)

	// Run cleanup
	tracker.Cleanup()

	// Recent entry should be preserved (not old enough to cleanup)
	if len(tracker.degradedIPs) != 1 {
		t.Errorf("expected 1 degraded IP after cleanup (recent entry), got %d", len(tracker.degradedIPs))
	}
}

func TestIPReputationTracker_WebhookAlert_Degraded(t *testing.T) {
	// Create test HTTP server to receive alerts
	var receivedAlert *ReputationAlert
	var mu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			t.Errorf("expected POST request, got %s", r.Method)
		}

		// Verify content type
		if r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("expected Content-Type application/json, got %s", r.Header.Get("Content-Type"))
		}

		// Verify auth header
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Errorf("expected Authorization 'Bearer test-token', got %s", r.Header.Get("Authorization"))
		}

		body, _ := io.ReadAll(r.Body)
		var alert ReputationAlert
		if err := json.Unmarshal(body, &alert); err != nil {
			t.Errorf("failed to unmarshal alert: %v", err)
		}

		mu.Lock()
		receivedAlert = &alert
		mu.Unlock()

		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	cfg := &config.ReputationConfig{
		EnableIPTracking:       true,
		AlertWebhookURL:        server.URL,
		AlertAuthToken:         "test-token",
		AlertTimeoutSeconds:    10,
		DegradedRetryHours:     48,
		DegradedIPCleanupHours: 168,
	}
	tracker := setupTestReputationTracker(t, cfg)

	deliveryInfo := DeliveryInfo{
		From:           "sender@example.com",
		To:             "recipient@example.com",
		Subject:        "Test Subject",
		IdempotencyKey: "test-key-123",
		MXHost:         "mx.example.com",
	}

	tracker.MarkIPDegraded("192.168.1.100", 550, "IP blocked by Spamhaus", deliveryInfo)

	// Wait for webhook to be sent (runs in background)
	time.Sleep(100 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()

	if receivedAlert == nil {
		t.Fatal("no alert received")
	}

	if receivedAlert.SourceIP != "192.168.1.100" {
		t.Errorf("expected source IP 192.168.1.100, got %s", receivedAlert.SourceIP)
	}

	if receivedAlert.EventType != "degraded" {
		t.Errorf("expected event type 'degraded', got %s", receivedAlert.EventType)
	}

	if receivedAlert.SMTPCode != 550 {
		t.Errorf("expected SMTP code 550, got %d", receivedAlert.SMTPCode)
	}

	if receivedAlert.SMTPResponse != "IP blocked by Spamhaus" {
		t.Errorf("expected SMTP response 'IP blocked by Spamhaus', got %s", receivedAlert.SMTPResponse)
	}

	if receivedAlert.From != "sender@example.com" {
		t.Errorf("expected from 'sender@example.com', got %s", receivedAlert.From)
	}

	if receivedAlert.To != "recipient@example.com" {
		t.Errorf("expected to 'recipient@example.com', got %s", receivedAlert.To)
	}

	if receivedAlert.Subject != "Test Subject" {
		t.Errorf("expected subject 'Test Subject', got %s", receivedAlert.Subject)
	}

	if receivedAlert.IdempotencyKey != "test-key-123" {
		t.Errorf("expected idempotency key 'test-key-123', got %s", receivedAlert.IdempotencyKey)
	}

	if receivedAlert.MXHost != "mx.example.com" {
		t.Errorf("expected MX host 'mx.example.com', got %s", receivedAlert.MXHost)
	}

	if receivedAlert.DegradedIPsCount != 1 {
		t.Errorf("expected degraded IPs count 1, got %d", receivedAlert.DegradedIPsCount)
	}
}

func TestIPReputationTracker_WebhookAlert_Recovered(t *testing.T) {
	var receivedAlert *ReputationAlert
	var mu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var alert ReputationAlert
		json.Unmarshal(body, &alert)

		mu.Lock()
		receivedAlert = &alert
		mu.Unlock()

		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	cfg := &config.ReputationConfig{
		EnableIPTracking:       true,
		AlertWebhookURL:        server.URL,
		AlertAuthToken:         "test-token",
		AlertTimeoutSeconds:    10,
		DegradedRetryHours:     48,
		DegradedIPCleanupHours: 168,
	}
	tracker := setupTestReputationTracker(t, cfg)

	deliveryInfo := DeliveryInfo{
		From:    "sender@example.com",
		To:      "recipient@example.com",
		Subject: "Test",
		MXHost:  "mx.example.com",
	}

	// Mark as degraded first
	tracker.MarkIPDegraded("192.168.1.100", 550, "Blocked", deliveryInfo)
	time.Sleep(50 * time.Millisecond)

	// Clear the degraded alert
	mu.Lock()
	receivedAlert = nil
	mu.Unlock()

	// Mark as recovered
	tracker.MarkIPRecovered("192.168.1.100")
	time.Sleep(100 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()

	if receivedAlert == nil {
		t.Fatal("no recovery alert received")
	}

	if receivedAlert.SourceIP != "192.168.1.100" {
		t.Errorf("expected source IP 192.168.1.100, got %s", receivedAlert.SourceIP)
	}

	if receivedAlert.EventType != "recovered" {
		t.Errorf("expected event type 'recovered', got %s", receivedAlert.EventType)
	}

	if receivedAlert.DegradedIPsCount != 0 {
		t.Errorf("expected degraded IPs count 0 after recovery, got %d", receivedAlert.DegradedIPsCount)
	}
}

func TestIPReputationTracker_NoWebhook(t *testing.T) {
	// No webhook configured - should not panic
	cfg := &config.ReputationConfig{
		EnableIPTracking:       true,
		AlertWebhookURL:        "", // No webhook
		AlertTimeoutSeconds:    10,
		DegradedRetryHours:     48,
		DegradedIPCleanupHours: 168,
	}
	tracker := setupTestReputationTracker(t, cfg)

	deliveryInfo := DeliveryInfo{
		From:    "sender@example.com",
		To:      "recipient@example.com",
		Subject: "Test",
		MXHost:  "mx.example.com",
	}

	// Should not panic
	tracker.MarkIPDegraded("192.168.1.100", 550, "Blocked", deliveryInfo)
	tracker.MarkIPRecovered("192.168.1.100")

	// Give time for any background goroutines
	time.Sleep(50 * time.Millisecond)
}

func TestIPReputationTracker_ConcurrentAccess(t *testing.T) {
	tracker := setupTestReputationTracker(t, nil)

	deliveryInfo := DeliveryInfo{
		From:    "sender@example.com",
		To:      "recipient@example.com",
		Subject: "Test",
		MXHost:  "mx.example.com",
	}

	var wg sync.WaitGroup
	numGoroutines := 100

	// Concurrent degradations
	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			ip := "192.168.1.100"
			if idx%2 == 0 {
				tracker.MarkIPDegraded(ip, 550, "Blocked", deliveryInfo)
			} else {
				tracker.IsIPHealthy(ip)
			}
		}(i)
	}

	wg.Wait()

	// Should not race or panic
	degradedIPs := tracker.GetDegradedIPs()
	if len(degradedIPs) != 1 {
		t.Errorf("expected 1 degraded IP after concurrent access, got %d", len(degradedIPs))
	}
}

func TestIPReputationTracker_GetDegradedIPs_Copy(t *testing.T) {
	tracker := setupTestReputationTracker(t, nil)

	deliveryInfo := DeliveryInfo{
		From:    "sender@example.com",
		To:      "recipient@example.com",
		Subject: "Test",
		MXHost:  "mx.example.com",
	}

	tracker.MarkIPDegraded("192.168.1.100", 550, "Blocked", deliveryInfo)

	degradedIPs := tracker.GetDegradedIPs()

	// Modify returned map (should not affect internal state)
	delete(degradedIPs, "192.168.1.100")

	// Internal state should be unchanged
	internalDegraded := tracker.GetDegradedIPs()
	if len(internalDegraded) != 1 {
		t.Errorf("expected 1 degraded IP in internal state, got %d", len(internalDegraded))
	}
}

func TestIPReputationTracker_EmptyIPString(t *testing.T) {
	tracker := setupTestReputationTracker(t, nil)

	deliveryInfo := DeliveryInfo{
		From:    "sender@example.com",
		To:      "recipient@example.com",
		Subject: "Test",
		MXHost:  "mx.example.com",
	}

	// Empty IP should not cause issues
	err := &DeliveryError{
		Category:     ErrorReputation,
		SMTPCode:     550,
		SMTPResponse: "Blocked",
		Message:      "IP reputation error",
	}

	tracker.RecordDeliveryAttempt("", false, err, deliveryInfo)

	// Should not track empty IP
	degradedIPs := tracker.GetDegradedIPs()
	if len(degradedIPs) != 0 {
		t.Errorf("expected 0 degraded IPs for empty IP string, got %d", len(degradedIPs))
	}
}

// Integration tests with delivery system

func TestDelivery_Integration_ReputationTracking(t *testing.T) {
	logger := zap.NewNop()

	// Create test webhook server
	var receivedAlerts []ReputationAlert
	var mu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var alert ReputationAlert
		json.Unmarshal(body, &alert)

		mu.Lock()
		receivedAlerts = append(receivedAlerts, alert)
		mu.Unlock()

		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	cfg := &config.DeliveryConfig{
		SourceIPs:                []string{"192.168.1.100", "192.168.1.101"},
		SourceIPSelection:              "round-robin",
		ConnectionTimeoutSeconds: 1,
		SMTPTimeoutSeconds:       1,
	}

	reputationCfg := &config.ReputationConfig{
		EnableIPTracking:       true,
		AlertWebhookURL:        server.URL,
		AlertTimeoutSeconds:    5,
		DegradedRetryHours:     48,
		DegradedIPCleanupHours: 168,
	}

	q, cleanup := queue.SetupTestQueue(t)
	defer cleanup()

	dnsCfg := &config.DNSConfig{}
	mxLookup := NewMXLookup(q, dnsCfg, cfg, logger)
	deliverer := NewDeliverer(cfg, mxLookup, logger, reputationCfg)

	// Test 1: All IPs should be healthy initially
	allIPs := deliverer.ipRotator.GetAllIPs()
	healthyIPs := deliverer.reputationTracker.GetHealthyIPs(allIPs)
	if len(healthyIPs) != 2 {
		t.Errorf("expected 2 healthy IPs initially, got %d", len(healthyIPs))
	}

	// Test 2: Simulate reputation failure
	deliveryInfo := DeliveryInfo{
		From:    "sender@example.com",
		To:      "recipient@example.com",
		Subject: "Test",
		MXHost:  "mx.example.com",
	}

	reputationErr := &DeliveryError{
		Category:     ErrorReputation,
		SMTPCode:     550,
		SMTPResponse: "Blocked by Spamhaus",
		Message:      "IP reputation error",
	}

	deliverer.reputationTracker.RecordDeliveryAttempt("192.168.1.100", false, reputationErr, deliveryInfo)

	// Wait for webhook
	time.Sleep(100 * time.Millisecond)

	// Test 3: IP should be degraded
	healthyIPs = deliverer.reputationTracker.GetHealthyIPs(allIPs)
	if len(healthyIPs) != 1 {
		t.Errorf("expected 1 healthy IP after degradation, got %d", len(healthyIPs))
	}

	if healthyIPs[0] != "192.168.1.101" {
		t.Errorf("expected healthy IP to be 192.168.1.101, got %s", healthyIPs[0])
	}

	// Test 4: Verify webhook alert was sent
	mu.Lock()
	alertCount := len(receivedAlerts)
	mu.Unlock()

	if alertCount != 1 {
		t.Errorf("expected 1 alert, got %d", alertCount)
	}

	// Test 5: Recovery
	deliverer.reputationTracker.RecordDeliveryAttempt("192.168.1.100", true, nil, deliveryInfo)

	time.Sleep(100 * time.Millisecond)

	healthyIPs = deliverer.reputationTracker.GetHealthyIPs(allIPs)
	if len(healthyIPs) != 2 {
		t.Errorf("expected 2 healthy IPs after recovery, got %d", len(healthyIPs))
	}

	// Test 6: Verify recovery alert was sent
	mu.Lock()
	alertCount = len(receivedAlerts)
	mu.Unlock()

	if alertCount != 2 {
		t.Errorf("expected 2 alerts (degraded + recovered), got %d", alertCount)
	}
}

func TestDelivery_Integration_AllIPsDegraded(t *testing.T) {
	logger := zap.NewNop()

	cfg := &config.DeliveryConfig{
		SourceIPs:                []string{"192.168.1.100", "192.168.1.101"},
		SourceIPSelection:              "round-robin",
		ConnectionTimeoutSeconds: 1,
		SMTPTimeoutSeconds:       1,
	}

	reputationCfg := &config.ReputationConfig{
		EnableIPTracking:       true,
		AlertTimeoutSeconds:    5,
		DegradedRetryHours:     48,
		DegradedIPCleanupHours: 168,
	}

	q, cleanup := queue.SetupTestQueue(t)
	defer cleanup()

	dnsCfg := &config.DNSConfig{}
	mxLookup := NewMXLookup(q, dnsCfg, cfg, logger)
	deliverer := NewDeliverer(cfg, mxLookup, logger, reputationCfg)

	deliveryInfo := DeliveryInfo{
		From:    "sender@example.com",
		To:      "recipient@example.com",
		Subject: "Test",
		MXHost:  "mx.example.com",
	}

	reputationErr := &DeliveryError{
		Category:     ErrorReputation,
		SMTPCode:     550,
		SMTPResponse: "Blocked",
		Message:      "IP reputation error",
	}

	// Degrade all IPs
	deliverer.reputationTracker.RecordDeliveryAttempt("192.168.1.100", false, reputationErr, deliveryInfo)
	deliverer.reputationTracker.RecordDeliveryAttempt("192.168.1.101", false, reputationErr, deliveryInfo)

	// When all IPs are degraded, should fall back to empty list (system default IP)
	allIPs := deliverer.ipRotator.GetAllIPs()
	healthyIPs := deliverer.reputationTracker.GetHealthyIPs(allIPs)

	if len(healthyIPs) != 0 {
		t.Errorf("expected 0 healthy IPs when all are degraded, got %d", len(healthyIPs))
	}

	// Verify degraded count
	degradedIPs := deliverer.reputationTracker.GetDegradedIPs()
	if len(degradedIPs) != 2 {
		t.Errorf("expected 2 degraded IPs, got %d", len(degradedIPs))
	}
}

func TestDelivery_Integration_NonReputationError(t *testing.T) {
	logger := zap.NewNop()

	cfg := &config.DeliveryConfig{
		SourceIPs:                []string{"192.168.1.100"},
		SourceIPSelection:              "round-robin",
		ConnectionTimeoutSeconds: 1,
		SMTPTimeoutSeconds:       1,
	}

	reputationCfg := &config.ReputationConfig{
		EnableIPTracking:       true,
		AlertTimeoutSeconds:    5,
		DegradedRetryHours:     48,
		DegradedIPCleanupHours: 168,
	}

	q, cleanup := queue.SetupTestQueue(t)
	defer cleanup()

	dnsCfg := &config.DNSConfig{}
	mxLookup := NewMXLookup(q, dnsCfg, cfg, logger)
	deliverer := NewDeliverer(cfg, mxLookup, logger, reputationCfg)

	deliveryInfo := DeliveryInfo{
		From:    "sender@example.com",
		To:      "recipient@example.com",
		Subject: "Test",
		MXHost:  "mx.example.com",
	}

	// Test various non-reputation errors
	testErrors := []*DeliveryError{
		{Category: ErrorTemporary, SMTPCode: 421, SMTPResponse: "Try later"},
		{Category: ErrorPermanent, SMTPCode: 550, SMTPResponse: "User not found"},
		{Category: ErrorNetwork, SMTPCode: 0, SMTPResponse: "Connection failed"},
		{Category: ErrorGreylist, SMTPCode: 421, SMTPResponse: "Greylisted"},
		{Category: ErrorThrottled, SMTPCode: 0, SMTPResponse: "Rate limited"},
	}

	for _, err := range testErrors {
		deliverer.reputationTracker.RecordDeliveryAttempt("192.168.1.100", false, err, deliveryInfo)

		// IP should remain healthy for non-reputation errors
		if !deliverer.reputationTracker.IsIPHealthy("192.168.1.100") {
			t.Errorf("IP should remain healthy for error category %s", err.Category)
		}

		degradedIPs := deliverer.reputationTracker.GetDegradedIPs()
		if len(degradedIPs) != 0 {
			t.Errorf("expected 0 degraded IPs for error category %s, got %d", err.Category, len(degradedIPs))
		}
	}
}

func TestDelivery_Integration_ReputationTrackingDisabled(t *testing.T) {
	logger := zap.NewNop()

	cfg := &config.DeliveryConfig{
		SourceIPs:                []string{"192.168.1.100", "192.168.1.101"},
		SourceIPSelection:              "round-robin",
		ConnectionTimeoutSeconds: 1,
		SMTPTimeoutSeconds:       1,
	}

	reputationCfg := &config.ReputationConfig{
		EnableIPTracking: false, // DISABLED
	}

	q, cleanup := queue.SetupTestQueue(t)
	defer cleanup()

	dnsCfg := &config.DNSConfig{}
	mxLookup := NewMXLookup(q, dnsCfg, cfg, logger)
	deliverer := NewDeliverer(cfg, mxLookup, logger, reputationCfg)

	deliveryInfo := DeliveryInfo{
		From:    "sender@example.com",
		To:      "recipient@example.com",
		Subject: "Test",
		MXHost:  "mx.example.com",
	}

	reputationErr := &DeliveryError{
		Category:     ErrorReputation,
		SMTPCode:     550,
		SMTPResponse: "Blocked",
		Message:      "IP reputation error",
	}

	// Even with reputation errors, IPs should remain healthy when tracking is disabled
	deliverer.reputationTracker.RecordDeliveryAttempt("192.168.1.100", false, reputationErr, deliveryInfo)

	allIPs := deliverer.ipRotator.GetAllIPs()
	healthyIPs := deliverer.reputationTracker.GetHealthyIPs(allIPs)

	if len(healthyIPs) != 2 {
		t.Errorf("expected 2 healthy IPs when tracking disabled, got %d", len(healthyIPs))
	}

	degradedIPs := deliverer.reputationTracker.GetDegradedIPs()
	if len(degradedIPs) != 0 {
		t.Errorf("expected 0 degraded IPs when tracking disabled, got %d", len(degradedIPs))
	}
}

func TestIPReputationTracker_MultipleIPsDifferentStates(t *testing.T) {
	tracker := setupTestReputationTracker(t, nil)

	deliveryInfo := DeliveryInfo{
		From:    "sender@example.com",
		To:      "recipient@example.com",
		Subject: "Test",
		MXHost:  "mx.example.com",
	}

	ips := []string{
		"192.168.1.100",
		"192.168.1.101",
		"192.168.1.102",
		"192.168.1.103",
		"192.168.1.104",
	}

	// Degrade some IPs
	tracker.MarkIPDegraded(ips[0], 550, "Spamhaus", deliveryInfo)
	tracker.MarkIPDegraded(ips[1], 550, "Barracuda", deliveryInfo)
	tracker.MarkIPDegraded(ips[3], 554, "Proofpoint", deliveryInfo)

	// Check individual states
	if tracker.IsIPHealthy(ips[0]) {
		t.Error("IP 0 should be degraded")
	}
	if tracker.IsIPHealthy(ips[1]) {
		t.Error("IP 1 should be degraded")
	}
	if !tracker.IsIPHealthy(ips[2]) {
		t.Error("IP 2 should be healthy")
	}
	if tracker.IsIPHealthy(ips[3]) {
		t.Error("IP 3 should be degraded")
	}
	if !tracker.IsIPHealthy(ips[4]) {
		t.Error("IP 4 should be healthy")
	}

	// Get healthy IPs
	healthyIPs := tracker.GetHealthyIPs(ips)
	if len(healthyIPs) != 2 {
		t.Errorf("expected 2 healthy IPs, got %d", len(healthyIPs))
	}

	// Verify correct IPs are healthy
	healthyMap := make(map[string]bool)
	for _, ip := range healthyIPs {
		healthyMap[ip] = true
	}

	if !healthyMap[ips[2]] {
		t.Error("IP 2 should be in healthy list")
	}
	if !healthyMap[ips[4]] {
		t.Error("IP 4 should be in healthy list")
	}

	// Get degraded IPs
	degradedIPs := tracker.GetDegradedIPs()
	if len(degradedIPs) != 3 {
		t.Errorf("expected 3 degraded IPs, got %d", len(degradedIPs))
	}

	// Recover one IP
	tracker.MarkIPRecovered(ips[1])

	// Should now have 3 healthy, 2 degraded
	healthyIPs = tracker.GetHealthyIPs(ips)
	if len(healthyIPs) != 3 {
		t.Errorf("expected 3 healthy IPs after recovery, got %d", len(healthyIPs))
	}

	degradedIPs = tracker.GetDegradedIPs()
	if len(degradedIPs) != 2 {
		t.Errorf("expected 2 degraded IPs after recovery, got %d", len(degradedIPs))
	}
}
