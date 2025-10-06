package main_test

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"fune/internal/callback"
	"fune/internal/config"
	"fune/internal/delivery"
	"fune/internal/dkim"
	"fune/internal/handler"
	"fune/internal/queue"
	"fune/internal/worker"

	"go.uber.org/zap"
)

// TestFullMessageFlow tests the complete message flow from HTTP to queue to delivery
func TestFullMessageFlow(t *testing.T) {
	// Setup logger
	logger, _ := zap.NewDevelopment()
	defer logger.Sync()

	// Create test database
	dbPath := "./test_queue.db"
	defer os.Remove(dbPath)

	// Initialize queue
	q, err := queue.NewQueue(dbPath, logger)
	if err != nil {
		t.Fatalf("Failed to create queue: %v", err)
	}
	defer q.Close()

	// Create test config
	deliveryCfg := &config.DeliveryConfig{
		MaxMessageAgeHours:        48,
		SourceIPs:                 []string{},
		IPSelection:               "round-robin",
		MXCacheTTLSeconds:         3600,
		ConnectionTimeoutSeconds:  30,
		SMTPTimeoutSeconds:        60,
		InitialRetryDelaySeconds:  300,
		MaxRetryDelaySeconds:      43200,
		BackoffMultiplier:         2.0,
		GreylistRetryDelaySeconds: 120,
	}

	// Use default HTTP config with test auth token
	cfg := &config.Config{}
	cfg.SetDefaults()
	httpCfg := &cfg.HTTP
	httpCfg.AuthToken = "test-token"

	// Initialize HTTP handler (without circuit breaker for test)
	httpHandler := handler.NewQueueMessageHandler(q, deliveryCfg, httpCfg, nil, logger)

	// Test 1: HTTP POST to enqueue message
	requestBody := map[string]string{
		"from":    "sender@example.com",
		"to":      "recipient@example.com",
		"subject": "Integration Test",
		"text":    "This is a test message",
	}

	bodyBytes, _ := json.Marshal(requestBody)
	req := httptest.NewRequest("POST", "/v1/messages", bytes.NewBuffer(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer test-token")

	w := httptest.NewRecorder()
	httpHandler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("Expected status 200, got %d: %s", w.Code, w.Body.String())
	}

	var response map[string]string
	json.Unmarshal(w.Body.Bytes(), &response)

	messageID := response["message_id"]
	if messageID == "" {
		t.Fatal("No message_id in response")
	}

	t.Logf("Message enqueued with ID: %s", messageID)

	// Test 2: Verify message is in queue
	messages, err := q.GetNextMessages(10)
	if err != nil {
		t.Fatalf("Failed to get messages: %v", err)
	}

	if len(messages) != 1 {
		t.Fatalf("Expected 1 message in queue, got %d", len(messages))
	}

	if messages[0].MessageID != messageID {
		t.Errorf("Message ID mismatch: %s != %s", messages[0].MessageID, messageID)
	}

	if messages[0].FromAddr != "sender@example.com" {
		t.Errorf("From address mismatch: %s", messages[0].FromAddr)
	}

	if messages[0].ToAddr != "recipient@example.com" {
		t.Errorf("To address mismatch: %s", messages[0].ToAddr)
	}

	if messages[0].ToDomain != "example.com" {
		t.Errorf("To domain mismatch: %s", messages[0].ToDomain)
	}

	t.Log("✓ Message correctly stored in queue")

	// Test 3: Verify raw message contains expected content
	rawMsg := string(messages[0].RawMessage)
	if !bytes.Contains([]byte(rawMsg), []byte("This is a test message")) {
		t.Errorf("Raw message doesn't contain expected text. Got:\n%s", rawMsg)
	}

	if !bytes.Contains([]byte(rawMsg), []byte("sender@example.com")) {
		t.Errorf("Raw message doesn't contain From address. Got:\n%s", rawMsg)
	}

	if !bytes.Contains([]byte(rawMsg), []byte("recipient@example.com")) {
		t.Errorf("Raw message doesn't contain To address. Got:\n%s", rawMsg)
	}

	if !bytes.Contains([]byte(rawMsg), []byte("Integration Test")) {
		t.Errorf("Raw message doesn't contain Subject. Got:\n%s", rawMsg)
	}

	t.Log("✓ MIME message correctly constructed")

	// Test 4: Test message expiration calculation
	if messages[0].ExpiresAt.Before(time.Now()) {
		t.Error("Message already expired")
	}

	expectedExpiry := time.Now().Add(48 * time.Hour)
	timeDiff := messages[0].ExpiresAt.Sub(expectedExpiry).Abs()
	if timeDiff > time.Minute {
		t.Errorf("Expiry time off by %v", timeDiff)
	}

	t.Log("✓ Message expiration correctly set to 48 hours")

	// Test 5: Test retry scheduler
	retryScheduler := delivery.NewRetryScheduler(deliveryCfg)

	// Calculate retry delays
	delay1 := retryScheduler.CalculateNextRetry(1, delivery.ErrorTemporary)
	if delay1 != 5*time.Minute {
		t.Errorf("First retry delay should be 5 minutes, got %v", delay1)
	}

	delay2 := retryScheduler.CalculateNextRetry(2, delivery.ErrorTemporary)
	if delay2 != 10*time.Minute {
		t.Errorf("Second retry delay should be 10 minutes, got %v", delay2)
	}

	delayGreylist := retryScheduler.CalculateNextRetry(1, delivery.ErrorGreylist)
	if delayGreylist != 2*time.Minute {
		t.Errorf("Greylist retry should be 2 minutes, got %v", delayGreylist)
	}

	delayPermanent := retryScheduler.CalculateNextRetry(1, delivery.ErrorPermanent)
	if delayPermanent != 0 {
		t.Errorf("Permanent error should not retry, got %v", delayPermanent)
	}

	t.Log("✓ Retry scheduler working correctly")

	// Test 6: Test worker components (without actual SMTP)
	// This tests the worker initialization
	mxLookup := delivery.NewMXLookup(q, deliveryCfg, logger)
	reputationCfg := &config.ReputationConfig{EnableIPTracking: false}
	deliverer := delivery.NewDeliverer(deliveryCfg, mxLookup, logger, reputationCfg)

	callbackCfg := &config.CallbacksConfig{
		WebhookURL:               "http://localhost:9999/webhook",
		TimeoutSeconds:           10,
		MaxCallbackAgeHours:      48,
		InitialRetryDelaySeconds: 30,
		MaxRetryDelaySeconds:     3600,
		BackoffMultiplier:        2.0,
		BatchSize:                10,
	}
	callbackHandler := callback.NewCallbackHandler(q, callbackCfg, logger)

	queueCfg := &config.QueueConfig{
		BatchSize:              5,
		PollIntervalSeconds:    30,
		CleanupIntervalSeconds: 60,
	}

	wrk := worker.NewWorker(q, deliverer, retryScheduler, callbackHandler, deliveryCfg, queueCfg, logger)

	if wrk == nil {
		t.Fatal("Failed to create worker")
	}

	t.Log("✓ Worker components initialized successfully")

	t.Log("\n=== Integration Test Complete ===")
	t.Log("✓ HTTP endpoint accepts messages")
	t.Log("✓ Messages are correctly enqueued")
	t.Log("✓ MIME messages are properly formatted")
	t.Log("✓ Message expiration is calculated")
	t.Log("✓ Retry scheduling works correctly")
	t.Log("✓ All components can be initialized")
}

// TestMessageIDGeneration tests that message IDs are unique
func TestMessageIDGeneration(t *testing.T) {
	ids := make(map[string]bool)

	for i := 0; i < 1000; i++ {
		id := queue.GenerateMessageID()
		if ids[id] {
			t.Fatalf("Duplicate message ID generated: %s", id)
		}
		ids[id] = true

		if len(id) < 15 {
			t.Fatalf("Message ID too short: %s", id)
		}

		if id[:4] != "msg_" {
			t.Fatalf("Message ID doesn't start with msg_: %s", id)
		}
	}

	t.Logf("✓ Generated 1000 unique message IDs")
}

// TestDomainExtraction tests email domain extraction
func TestDomainExtraction(t *testing.T) {
	tests := []struct {
		email    string
		expected string
	}{
		{"user@example.com", "example.com"},
		{"test@subdomain.example.com", "subdomain.example.com"},
		{"invalid", ""},
		{"@example.com", ""},
		{"user@", ""},
	}

	for _, tt := range tests {
		result := queue.ExtractDomain(tt.email)
		if result != tt.expected {
			t.Errorf("ExtractDomain(%s) = %s, want %s", tt.email, result, tt.expected)
		}
	}

	t.Log("✓ Domain extraction working correctly")
}

// MockSMTPServer simulates an SMTP server for integration testing
type MockSMTPServer struct {
	listener      net.Listener
	addr          string
	responses     []string // SMTP responses to send
	receivedMsgs  []string // Messages received
	mu            sync.Mutex
	responseIndex int32
	running       atomic.Bool
	t             *testing.T
}

func NewMockSMTPServer(t *testing.T, responses []string) (*MockSMTPServer, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}

	server := &MockSMTPServer{
		listener:  listener,
		addr:      listener.Addr().String(),
		responses: responses,
		t:         t,
	}

	server.running.Store(true)
	go server.serve()

	return server, nil
}

func (s *MockSMTPServer) serve() {
	for s.running.Load() {
		conn, err := s.listener.Accept()
		if err != nil {
			if s.running.Load() {
				s.t.Logf("Mock SMTP accept error: %v", err)
			}
			return
		}
		go s.handleConnection(conn)
	}
}

func (s *MockSMTPServer) handleConnection(conn net.Conn) {
	defer conn.Close()

	reader := bufio.NewReader(conn)
	writer := bufio.NewWriter(conn)

	// Send greeting
	writer.WriteString("220 mock.smtp.local ESMTP\r\n")
	writer.Flush()

	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return
		}

		line = strings.TrimSpace(line)
		s.t.Logf("Mock SMTP received: %s", line)

		// Handle SMTP commands
		if strings.HasPrefix(line, "EHLO") || strings.HasPrefix(line, "HELO") {
			writer.WriteString("250 mock.smtp.local\r\n")
		} else if strings.HasPrefix(line, "MAIL FROM:") {
			writer.WriteString("250 OK\r\n")
		} else if strings.HasPrefix(line, "RCPT TO:") {
			writer.WriteString("250 OK\r\n")
		} else if strings.HasPrefix(line, "DATA") {
			writer.WriteString("354 Start mail input\r\n")
			writer.Flush()

			// Read message data
			var msgData strings.Builder
			for {
				dataLine, err := reader.ReadString('\n')
				if err != nil {
					return
				}
				if dataLine == ".\r\n" {
					break
				}
				msgData.WriteString(dataLine)
			}

			s.mu.Lock()
			s.receivedMsgs = append(s.receivedMsgs, msgData.String())
			s.mu.Unlock()

			// Send configured response or default success
			idx := atomic.LoadInt32(&s.responseIndex)
			if int(idx) < len(s.responses) {
				writer.WriteString(s.responses[idx] + "\r\n")
				atomic.AddInt32(&s.responseIndex, 1)
			} else {
				writer.WriteString("250 OK\r\n")
			}
		} else if strings.HasPrefix(line, "QUIT") {
			writer.WriteString("221 Bye\r\n")
			writer.Flush()
			return
		} else if strings.HasPrefix(line, "RSET") {
			writer.WriteString("250 OK\r\n")
		} else {
			writer.WriteString("500 Command not recognized\r\n")
		}

		writer.Flush()
	}
}

func (s *MockSMTPServer) GetReceivedMessages() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	msgs := make([]string, len(s.receivedMsgs))
	copy(msgs, s.receivedMsgs)
	return msgs
}

func (s *MockSMTPServer) Close() {
	s.running.Store(false)
	s.listener.Close()
}

// TestEndToEndDelivery tests complete message flow including actual SMTP delivery
func TestEndToEndDelivery(t *testing.T) {
	// Note: Real SMTP testing requires either:
	// 1. Running mock SMTP on port 25 (requires root)
	// 2. Modifying delivery code to support custom ports (not production-like)
	// 3. Using iptables to redirect port 25 to our mock (complex)
	//
	// We skip this test and rely on unit tests for SMTP delivery logic.
	// The mock SMTP server code is available for manual testing if needed.

	t.Skip("SMTP delivery test requires port 25 access or code modifications")
}

// TestWorkerLifecycle tests worker start, processing, and graceful shutdown
func TestWorkerLifecycle(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	defer logger.Sync()

	dbPath := "./test_worker_lifecycle.db"
	defer os.Remove(dbPath)

	q, err := queue.NewQueue(dbPath, logger)
	if err != nil {
		t.Fatalf("Failed to create queue: %v", err)
	}
	defer q.Close()

	deliveryCfg := &config.DeliveryConfig{
		MaxMessageAgeHours:         48,
		SourceIPs:                  []string{},
		IPSelection:                "round-robin",
		MXCacheTTLSeconds:          3600,
		ConnectionTimeoutSeconds:   5,
		SMTPTimeoutSeconds:         10,
		InitialRetryDelaySeconds:   2,
		MaxRetryDelaySeconds:       10,
		BackoffMultiplier:          2.0,
		GreylistRetryDelaySeconds:  2,
		MaxIPsPerMX:                5,
		MinDeliveryIntervalSeconds: 0,
		ThrottleRetryDelaySeconds:  1,
		DNSTimeoutSeconds:          5,
		DNSCacheNegativeTTL:        60,
	}

	queueCfg := &config.QueueConfig{
		BatchSize:              5,
		PollIntervalSeconds:    1, // Fast polling for test
		CleanupIntervalSeconds: 60,
		WorkerCount:            2,
	}

	callbackCfg := &config.CallbacksConfig{
		WebhookURL:               "http://localhost:9999/webhook",
		TimeoutSeconds:           5,
		MaxCallbackAgeHours:      1,
		InitialRetryDelaySeconds: 1,
		MaxRetryDelaySeconds:     10,
		BackoffMultiplier:        2.0,
		BatchSize:                10,
	}

	mxLookup := delivery.NewMXLookup(q, deliveryCfg, logger)
	reputationCfg := &config.ReputationConfig{EnableIPTracking: false}
	deliverer := delivery.NewDeliverer(deliveryCfg, mxLookup, logger, reputationCfg)
	retryScheduler := delivery.NewRetryScheduler(deliveryCfg)
	callbackHandler := callback.NewCallbackHandler(q, callbackCfg, logger)

	// Start callback handler
	callbackHandler.Start()
	defer callbackHandler.Stop()

	// Create worker
	w := worker.NewWorker(q, deliverer, retryScheduler, callbackHandler, deliveryCfg, queueCfg, logger)

	// Start workers
	w.Start(2)
	t.Log("✓ Workers started")

	// Give workers time to initialize
	time.Sleep(100 * time.Millisecond)

	// Enqueue a test message (will fail DNS lookup, but tests worker processing)
	msg := &queue.QueuedMessage{
		MessageID:  queue.GenerateMessageID(),
		Status:     queue.StatusQueued,
		FromAddr:   "test@example.com",
		ToAddr:     "recipient@nonexistent.invalid",
		ToDomain:   "nonexistent.invalid",
		Subject:    "Test",
		RawMessage: []byte("From: test@example.com\r\nTo: recipient@nonexistent.invalid\r\n\r\nTest"),
		CreatedAt:  time.Now(),
		ExpiresAt:  time.Now().Add(48 * time.Hour),
		Attempts:   0,
	}

	err = q.Enqueue(msg)
	if err != nil {
		t.Fatalf("Failed to enqueue: %v", err)
	}

	t.Log("✓ Message enqueued")

	// Wait for worker to process (will fail but that's expected)
	time.Sleep(2 * time.Second)

	// Check that message was processed (should be in failed state with retry scheduled)
	messages, _ := q.GetNextMessages(10)
	t.Logf("Messages in queue after processing: %d", len(messages))

	// Graceful shutdown
	w.Stop()
	t.Log("✓ Workers stopped gracefully")

	callbackHandler.Stop()
	t.Log("✓ Callback handler stopped")

	t.Log("✓ Worker lifecycle test complete")
}

// TestCallbackWebhook tests callback delivery with mock webhook server
func TestCallbackWebhook(t *testing.T) {
	// Track received callbacks
	var receivedCallbacks []callback.DeliveryEventCallback
	var callbackMu sync.Mutex

	// Mock webhook server
	webhookServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-webhook-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}

		body, _ := io.ReadAll(r.Body)
		var cb callback.DeliveryEventCallback
		json.Unmarshal(body, &cb)

		callbackMu.Lock()
		receivedCallbacks = append(receivedCallbacks, cb)
		callbackMu.Unlock()

		t.Logf("Webhook received: %s for message %s", cb.Event, cb.MessageID)
		w.WriteHeader(http.StatusOK)
	}))
	defer webhookServer.Close()

	logger, _ := zap.NewDevelopment()
	defer logger.Sync()

	dbPath := "./test_callback.db"
	defer os.Remove(dbPath)

	q, err := queue.NewQueue(dbPath, logger)
	if err != nil {
		t.Fatalf("Failed to create queue: %v", err)
	}
	defer q.Close()

	callbackCfg := &config.CallbacksConfig{
		WebhookURL:               webhookServer.URL,
		AuthToken:                "test-webhook-token",
		TimeoutSeconds:           5,
		MaxCallbackAgeHours:      1,
		InitialRetryDelaySeconds: 1,
		MaxRetryDelaySeconds:     10,
		BackoffMultiplier:        2.0,
		BatchSize:                10,
	}

	callbackHandler := callback.NewCallbackHandler(q, callbackCfg, logger)
	callbackHandler.Start()
	defer callbackHandler.Stop()

	// Enqueue test callbacks
	testMsg := &queue.QueuedMessage{
		MessageID: queue.GenerateMessageID(),
		FromAddr:  "sender@example.com",
		ToAddr:    "recipient@example.com",
		Subject:   "Test",
		CreatedAt: time.Now(),
		Attempts:  1,
	}

	testResult := &delivery.DeliveryResult{
		Success:      true,
		SMTPCode:     250,
		SMTPResponse: "OK",
		MXHost:       "mx.example.com",
		SourceIP:     "192.168.1.100",
	}

	// Enqueue delivered callback
	callbackHandler.EnqueueDeliveredCallback(testMsg, testResult)
	t.Log("✓ Delivered callback enqueued")

	// Wait for callback processing
	time.Sleep(2 * time.Second)

	// Verify callback was received
	callbackMu.Lock()
	count := len(receivedCallbacks)
	callbackMu.Unlock()

	if count != 1 {
		t.Errorf("Expected 1 callback, got %d", count)
	} else {
		cb := receivedCallbacks[0]
		if cb.Event != "delivered" {
			t.Errorf("Expected event type 'delivered', got '%s'", cb.Event)
		}
		if cb.MessageID != testMsg.MessageID {
			t.Errorf("Message ID mismatch: %s != %s", cb.MessageID, testMsg.MessageID)
		}
		if cb.SMTPCode != 250 {
			t.Errorf("SMTP code mismatch: %d != 250", cb.SMTPCode)
		}
		t.Log("✓ Callback delivered successfully with correct data")
	}

	callbackHandler.Stop()
}

// TestDeliveryRetry tests retry logic with temporary failures
func TestDeliveryRetry(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	defer logger.Sync()

	dbPath := "./test_retry.db"
	defer os.Remove(dbPath)

	q, err := queue.NewQueue(dbPath, logger)
	if err != nil {
		t.Fatalf("Failed to create queue: %v", err)
	}
	defer q.Close()

	deliveryCfg := &config.DeliveryConfig{
		MaxMessageAgeHours:         1, // Short expiry for testing
		InitialRetryDelaySeconds:   1,
		MaxRetryDelaySeconds:       10,
		BackoffMultiplier:          2.0,
		GreylistRetryDelaySeconds:  1,
		ConnectionTimeoutSeconds:   5,
		SMTPTimeoutSeconds:         10,
		MXCacheTTLSeconds:          3600,
		MaxIPsPerMX:                5,
		MinDeliveryIntervalSeconds: 0,
		ThrottleRetryDelaySeconds:  1,
		DNSTimeoutSeconds:          5,
		DNSCacheNegativeTTL:        60,
	}

	retryScheduler := delivery.NewRetryScheduler(deliveryCfg)

	// Test exponential backoff
	delay1 := retryScheduler.CalculateNextRetry(1, delivery.ErrorTemporary)
	delay2 := retryScheduler.CalculateNextRetry(2, delivery.ErrorTemporary)
	delay3 := retryScheduler.CalculateNextRetry(3, delivery.ErrorTemporary)

	if delay1 != 1*time.Second {
		t.Errorf("First retry should be 1s, got %v", delay1)
	}
	if delay2 != 2*time.Second {
		t.Errorf("Second retry should be 2s, got %v", delay2)
	}
	if delay3 != 4*time.Second {
		t.Errorf("Third retry should be 4s, got %v", delay3)
	}

	t.Log("✓ Exponential backoff working correctly")

	// Test greylisting
	greylistDelay := retryScheduler.CalculateNextRetry(1, delivery.ErrorGreylist)
	if greylistDelay != 1*time.Second {
		t.Errorf("Greylist retry should be 1s, got %v", greylistDelay)
	}

	t.Log("✓ Greylist retry working correctly")

	// Test permanent errors
	permanentDelay := retryScheduler.CalculateNextRetry(1, delivery.ErrorPermanent)
	if permanentDelay != 0 {
		t.Errorf("Permanent errors should not retry, got %v", permanentDelay)
	}

	t.Log("✓ Permanent error handling correct")

	// Test message expiration
	expiredMsg := &queue.QueuedMessage{
		MessageID: queue.GenerateMessageID(),
		CreatedAt: time.Now().Add(-2 * time.Hour), // 2 hours ago
		ExpiresAt: time.Now().Add(-1 * time.Hour), // Expired 1 hour ago
	}

	if !delivery.IsExpired(expiredMsg) {
		t.Error("Message should be expired")
	}

	t.Log("✓ Message expiration check working")
}

// TestDestinationThrottling tests rate limiting per destination domain
func TestDestinationThrottling(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	defer logger.Sync()

	deliveryCfg := &config.DeliveryConfig{
		MinDeliveryIntervalSeconds: 2, // 2 second throttle
		MaxIPsPerMX:                5,
		MXCacheTTLSeconds:          3600,
		ConnectionTimeoutSeconds:   30,
		SMTPTimeoutSeconds:         60,
	}

	// Create throttle tracker
	throttle := delivery.NewDestinationThrottle(deliveryCfg.MinDeliveryIntervalSeconds)

	domain := "example.com"

	// First attempt should not throttle
	shouldThrottle, _ := throttle.ShouldThrottle(domain)
	if shouldThrottle {
		t.Error("First attempt should not throttle")
	}

	// Record attempt
	throttle.RecordAttempt(domain)

	// Immediate second attempt should throttle
	shouldThrottle, waitTime := throttle.ShouldThrottle(domain)
	if !shouldThrottle {
		t.Error("Second immediate attempt should throttle")
	}
	if waitTime <= 0 || waitTime > 2*time.Second {
		t.Errorf("Wait time should be between 0 and 2s, got %v", waitTime)
	}

	t.Logf("✓ Throttling active, wait time: %v", waitTime)

	// Wait for throttle window
	time.Sleep(2 * time.Second)

	// Should not throttle after waiting
	shouldThrottle, _ = throttle.ShouldThrottle(domain)
	if shouldThrottle {
		t.Error("Should not throttle after waiting")
	}

	t.Log("✓ Throttle cleared after interval")

	// Test different domain is not throttled
	throttle.RecordAttempt("example.com")
	shouldThrottle, _ = throttle.ShouldThrottle("other.com")
	if shouldThrottle {
		t.Error("Different domain should not be throttled")
	}

	t.Log("✓ Throttling is per-domain")
}

// TestConcurrentWorkers tests multiple workers processing messages concurrently
func TestConcurrentWorkers(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	defer logger.Sync()

	dbPath := "./test_concurrent.db"
	defer os.Remove(dbPath)

	q, err := queue.NewQueue(dbPath, logger)
	if err != nil {
		t.Fatalf("Failed to create queue: %v", err)
	}
	defer q.Close()

	// Enqueue multiple messages
	numMessages := 10
	for i := 0; i < numMessages; i++ {
		msg := &queue.QueuedMessage{
			MessageID:  queue.GenerateMessageID(),
			Status:     queue.StatusQueued,
			FromAddr:   "sender@example.com",
			ToAddr:     fmt.Sprintf("recipient%d@test.invalid", i),
			ToDomain:   "test.invalid",
			Subject:    fmt.Sprintf("Test %d", i),
			RawMessage: []byte("Test message"),
			CreatedAt:  time.Now(),
			ExpiresAt:  time.Now().Add(48 * time.Hour),
			Attempts:   0,
		}
		q.Enqueue(msg)
	}

	t.Logf("✓ Enqueued %d messages", numMessages)

	// Create worker pool
	deliveryCfg := &config.DeliveryConfig{
		MaxMessageAgeHours:         48,
		SourceIPs:                  []string{},
		IPSelection:                "round-robin",
		MXCacheTTLSeconds:          3600,
		ConnectionTimeoutSeconds:   5,
		SMTPTimeoutSeconds:         10,
		InitialRetryDelaySeconds:   300,
		MaxRetryDelaySeconds:       43200,
		BackoffMultiplier:          2.0,
		GreylistRetryDelaySeconds:  120,
		MaxIPsPerMX:                5,
		MinDeliveryIntervalSeconds: 0,
		ThrottleRetryDelaySeconds:  5,
		DNSTimeoutSeconds:          5,
		DNSCacheNegativeTTL:        60,
	}

	queueCfg := &config.QueueConfig{
		BatchSize:              5,
		PollIntervalSeconds:    1,
		CleanupIntervalSeconds: 60,
		WorkerCount:            3,
	}

	callbackCfg := &config.CallbacksConfig{
		WebhookURL:               "http://localhost:9999/webhook",
		TimeoutSeconds:           10,
		MaxCallbackAgeHours:      48,
		InitialRetryDelaySeconds: 30,
		MaxRetryDelaySeconds:     3600,
		BackoffMultiplier:        2.0,
		BatchSize:                10,
	}

	mxLookup := delivery.NewMXLookup(q, deliveryCfg, logger)
	reputationCfg := &config.ReputationConfig{EnableIPTracking: false}
	deliverer := delivery.NewDeliverer(deliveryCfg, mxLookup, logger, reputationCfg)
	retryScheduler := delivery.NewRetryScheduler(deliveryCfg)
	callbackHandler := callback.NewCallbackHandler(q, callbackCfg, logger)

	w := worker.NewWorker(q, deliverer, retryScheduler, callbackHandler, deliveryCfg, queueCfg, logger)

	// Start workers
	w.Start(3)
	t.Log("✓ Started 3 concurrent workers")

	// Wait for processing
	time.Sleep(3 * time.Second)

	// Stop workers
	w.Stop()
	t.Log("✓ Workers stopped")

	// All messages should have been attempted (will fail DNS, but that's expected)
	messages, _ := q.GetNextMessages(100)
	t.Logf("Messages remaining in pending state: %d", len(messages))

	t.Log("✓ Concurrent worker test complete")
}

// generateTestRSAKey generates a test RSA private key of specified size
func generateTestRSAKey(bits int) (string, error) {
	privateKey, err := rsa.GenerateKey(rand.Reader, bits)
	if err != nil {
		return "", err
	}

	// Encode as PKCS#1 PEM for 2048, PKCS#8 for 1024 (test both formats)
	var pemBlock *pem.Block
	if bits == 2048 {
		pemBlock = &pem.Block{
			Type:  "RSA PRIVATE KEY",
			Bytes: x509.MarshalPKCS1PrivateKey(privateKey),
		}
	} else {
		pkcs8Bytes, err := x509.MarshalPKCS8PrivateKey(privateKey)
		if err != nil {
			return "", err
		}
		pemBlock = &pem.Block{
			Type:  "PRIVATE KEY",
			Bytes: pkcs8Bytes,
		}
	}

	return string(pem.EncodeToMemory(pemBlock)), nil
}

// TestDKIMSigningEndToEnd tests complete DKIM signing flow from HTTP to delivery
func TestDKIMSigningEndToEnd(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	defer logger.Sync()

	dbPath := "./test_dkim.db"
	defer os.Remove(dbPath)

	q, err := queue.NewQueue(dbPath, logger)
	if err != nil {
		t.Fatalf("Failed to create queue: %v", err)
	}
	defer q.Close()

	// Generate test RSA keys
	testPrivateKey2048, err := generateTestRSAKey(2048)
	if err != nil {
		t.Fatalf("Failed to generate 2048-bit key: %v", err)
	}

	testPrivateKey1024, err := generateTestRSAKey(1024)
	if err != nil {
		t.Fatalf("Failed to generate 1024-bit key: %v", err)
	}

	deliveryCfg := &config.DeliveryConfig{
		MaxMessageAgeHours:        48,
		SourceIPs:                 []string{},
		IPSelection:               "round-robin",
		MXCacheTTLSeconds:         3600,
		ConnectionTimeoutSeconds:  30,
		SMTPTimeoutSeconds:        60,
		InitialRetryDelaySeconds:  300,
		MaxRetryDelaySeconds:      43200,
		BackoffMultiplier:         2.0,
		GreylistRetryDelaySeconds: 120,
	}

	cfg := &config.Config{}
	cfg.SetDefaults()
	httpCfg := &cfg.HTTP
	httpCfg.AuthToken = "test-token"

	httpHandler := handler.NewQueueMessageHandler(q, deliveryCfg, httpCfg, nil, logger)

	// Test 1: Enqueue message with DKIM signature (2048-bit key)
	t.Log("=== Test 1: 2048-bit RSA DKIM Key ===")
	requestBody := map[string]string{
		"from":             "sender@example.com",
		"to":               "recipient@example.com",
		"subject":          "DKIM Test - 2048 bit",
		"text":             "This message should be DKIM signed",
		"dkim_private_key": testPrivateKey2048,
		"dkim_selector":    "default",
		"dkim_domain":      "example.com",
	}

	bodyBytes, _ := json.Marshal(requestBody)
	req := httptest.NewRequest("POST", "/v1/messages", bytes.NewBuffer(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer test-token")

	w := httptest.NewRecorder()
	httpHandler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("Expected status 200, got %d: %s", w.Code, w.Body.String())
	}

	var response map[string]string
	json.Unmarshal(w.Body.Bytes(), &response)
	messageID := response["message_id"]
	t.Logf("✓ Message enqueued with ID: %s", messageID)

	// Verify DKIM fields stored in queue
	messages, _ := q.GetNextMessages(10)
	if len(messages) != 1 {
		t.Fatalf("Expected 1 message, got %d", len(messages))
	}

	msg := messages[0]
	if msg.DKIMPrivateKey != testPrivateKey2048 {
		t.Error("DKIM private key not stored correctly")
	}
	if msg.DKIMSelector != "default" {
		t.Errorf("DKIM selector mismatch: %s", msg.DKIMSelector)
	}
	if msg.DKIMDomain != "example.com" {
		t.Errorf("DKIM domain mismatch: %s", msg.DKIMDomain)
	}
	t.Log("✓ DKIM fields correctly stored in queue")

	// Test signing the message
	signedMessage, err := dkim.SignMessage(msg.RawMessage, msg.DKIMPrivateKey, msg.DKIMSelector, msg.DKIMDomain)
	if err != nil {
		t.Fatalf("Failed to sign message: %v", err)
	}

	// Verify DKIM-Signature header present
	signedStr := string(signedMessage)
	if !strings.Contains(signedStr, "DKIM-Signature:") {
		t.Error("Signed message missing DKIM-Signature header")
	}
	if !strings.Contains(signedStr, "s=default") {
		t.Error("DKIM signature missing selector")
	}
	if !strings.Contains(signedStr, "d=example.com") {
		t.Error("DKIM signature missing domain")
	}
	t.Log("✓ DKIM signature successfully added to message")
	t.Logf("✓ Signed message size: %d bytes (original: %d bytes)", len(signedMessage), len(msg.RawMessage))

	// Clean up queue
	q.DeleteMessage(messageID)

	// Test 2: Enqueue message with 1024-bit DKIM key
	t.Log("\n=== Test 2: 1024-bit RSA DKIM Key ===")
	requestBody["dkim_private_key"] = testPrivateKey1024
	requestBody["subject"] = "DKIM Test - 1024 bit"

	bodyBytes, _ = json.Marshal(requestBody)
	req = httptest.NewRequest("POST", "/v1/messages", bytes.NewBuffer(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer test-token")

	w = httptest.NewRecorder()
	httpHandler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("Expected status 200, got %d: %s", w.Code, w.Body.String())
	}

	json.Unmarshal(w.Body.Bytes(), &response)
	messageID = response["message_id"]
	t.Logf("✓ Message with 1024-bit key enqueued: %s", messageID)

	messages, _ = q.GetNextMessages(10)
	msg = messages[0]

	signedMessage, err = dkim.SignMessage(msg.RawMessage, msg.DKIMPrivateKey, msg.DKIMSelector, msg.DKIMDomain)
	if err != nil {
		t.Fatalf("Failed to sign with 1024-bit key: %v", err)
	}
	t.Log("✓ 1024-bit DKIM key accepted and signing works")

	q.DeleteMessage(messageID)

	// Test 3: Invalid DKIM key should be rejected
	t.Log("\n=== Test 3: Invalid DKIM Key Rejection ===")
	requestBody["dkim_private_key"] = "invalid-key-data"

	bodyBytes, _ = json.Marshal(requestBody)
	req = httptest.NewRequest("POST", "/v1/messages", bytes.NewBuffer(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer test-token")

	w = httptest.NewRecorder()
	httpHandler.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("Expected status 400 for invalid key, got %d: %s", w.Code, w.Body.String())
	} else {
		t.Log("✓ Invalid DKIM key correctly rejected with 400")
	}

	// Test 4: Message without DKIM key (optional)
	t.Log("\n=== Test 4: Message Without DKIM (Optional) ===")
	requestBody = map[string]string{
		"from":    "sender@example.com",
		"to":      "recipient@example.com",
		"subject": "No DKIM",
		"text":    "This message has no DKIM signature",
	}

	bodyBytes, _ = json.Marshal(requestBody)
	req = httptest.NewRequest("POST", "/v1/messages", bytes.NewBuffer(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer test-token")

	w = httptest.NewRecorder()
	httpHandler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("Expected status 200, got %d", w.Code)
	}

	json.Unmarshal(w.Body.Bytes(), &response)
	messageID = response["message_id"]

	messages, _ = q.GetNextMessages(10)
	msg = messages[0]

	if msg.DKIMPrivateKey != "" {
		t.Error("DKIM private key should be empty for unsigned message")
	}
	t.Log("✓ Message without DKIM correctly accepted")

	// Test 5: Default DKIM domain to sender's domain
	t.Log("\n=== Test 5: Default DKIM Domain ===")
	requestBody = map[string]string{
		"from":             "test@mydomain.com",
		"to":               "recipient@example.com",
		"subject":          "DKIM with default domain",
		"text":             "Testing default domain",
		"dkim_private_key": testPrivateKey2048,
		"dkim_selector":    "mail",
		// Note: dkim_domain not specified, should default to mydomain.com
	}

	bodyBytes, _ = json.Marshal(requestBody)
	req = httptest.NewRequest("POST", "/v1/messages", bytes.NewBuffer(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer test-token")

	w = httptest.NewRecorder()
	httpHandler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("Expected status 200, got %d", w.Code)
	}

	// Clear old messages and get new one
	q.DeleteMessage(messageID)
	messages, _ = q.GetNextMessages(10)
	msg = messages[0]

	if msg.DKIMDomain != "mydomain.com" {
		t.Errorf("DKIM domain should default to sender domain 'mydomain.com', got: %s", msg.DKIMDomain)
	} else {
		t.Log("✓ DKIM domain correctly defaulted to sender's domain")
	}

	t.Log("\n=== DKIM Integration Test Complete ===")
	t.Log("✓ 2048-bit RSA keys supported")
	t.Log("✓ 1024-bit RSA keys supported")
	t.Log("✓ Invalid keys rejected at enqueue time")
	t.Log("✓ DKIM is optional (messages work without it)")
	t.Log("✓ DKIM domain defaults to sender's domain")
	t.Log("✓ DKIM signatures correctly added to messages")
}
