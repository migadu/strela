package delivery

import (
	"testing"
	"time"

	"fune/internal/config"
	"fune/internal/queue"
)

func TestRetryScheduler_CalculateNextRetry_Permanent(t *testing.T) {
	cfg := &config.DeliveryConfig{
		InitialRetryDelaySeconds:  300,
		MaxRetryDelaySeconds:      43200,
		BackoffMultiplier:         2.0,
		GreylistRetryDelaySeconds: 120,
	}

	scheduler := NewRetryScheduler(cfg)

	// Permanent errors should not retry
	delay := scheduler.CalculateNextRetry(1, ErrorPermanent)
	if delay != 0 {
		t.Errorf("Permanent errors should have 0 delay, got %v", delay)
	}
}

func TestRetryScheduler_CalculateNextRetry_Greylist(t *testing.T) {
	cfg := &config.DeliveryConfig{
		InitialRetryDelaySeconds:  300,
		MaxRetryDelaySeconds:      43200,
		BackoffMultiplier:         2.0,
		GreylistRetryDelaySeconds: 120,
	}

	scheduler := NewRetryScheduler(cfg)

	// Greylist should use fixed short delay
	delay := scheduler.CalculateNextRetry(1, ErrorGreylist)
	expected := 120 * time.Second

	if delay != expected {
		t.Errorf("Expected greylist delay %v, got %v", expected, delay)
	}

	// Should be same for all attempts
	delay2 := scheduler.CalculateNextRetry(5, ErrorGreylist)
	if delay2 != expected {
		t.Errorf("Greylist delay should be constant, got %v on attempt 5", delay2)
	}
}

func TestRetryScheduler_CalculateNextRetry_ExponentialBackoff(t *testing.T) {
	cfg := &config.DeliveryConfig{
		InitialRetryDelaySeconds:  300,  // 5 minutes
		MaxRetryDelaySeconds:      43200, // 12 hours
		BackoffMultiplier:         2.0,
		GreylistRetryDelaySeconds: 120,
	}

	scheduler := NewRetryScheduler(cfg)

	tests := []struct {
		attempt  int
		expected time.Duration
	}{
		{1, 300 * time.Second},   // 5 min
		{2, 600 * time.Second},   // 10 min
		{3, 1200 * time.Second},  // 20 min
		{4, 2400 * time.Second},  // 40 min
		{5, 4800 * time.Second},  // 80 min
		{6, 9600 * time.Second},  // 160 min
		{7, 19200 * time.Second}, // 320 min
	}

	for _, tt := range tests {
		delay := scheduler.CalculateNextRetry(tt.attempt, ErrorTemporary)
		if delay != tt.expected {
			t.Errorf("Attempt %d: expected delay %v, got %v", tt.attempt, tt.expected, delay)
		}
	}
}

func TestRetryScheduler_CalculateNextRetry_MaxDelayCap(t *testing.T) {
	cfg := &config.DeliveryConfig{
		InitialRetryDelaySeconds:  300,
		MaxRetryDelaySeconds:      43200, // 12 hours
		BackoffMultiplier:         2.0,
		GreylistRetryDelaySeconds: 120,
	}

	scheduler := NewRetryScheduler(cfg)

	// After enough attempts, delay should cap at max
	maxDelay := time.Duration(cfg.MaxRetryDelaySeconds) * time.Second

	delay10 := scheduler.CalculateNextRetry(10, ErrorTemporary)
	if delay10 != maxDelay {
		t.Errorf("Expected max delay %v after 10 attempts, got %v", maxDelay, delay10)
	}

	delay20 := scheduler.CalculateNextRetry(20, ErrorTemporary)
	if delay20 != maxDelay {
		t.Errorf("Expected max delay %v after 20 attempts, got %v", maxDelay, delay20)
	}
}

func TestRetryScheduler_ShouldRetry_Permanent(t *testing.T) {
	cfg := &config.DeliveryConfig{
		InitialRetryDelaySeconds:  300,
		MaxRetryDelaySeconds:      43200,
		BackoffMultiplier:         2.0,
		GreylistRetryDelaySeconds: 120,
	}

	scheduler := NewRetryScheduler(cfg)

	msg := &queue.QueuedMessage{
		MessageID: "test",
		ExpiresAt: time.Now().Add(24 * time.Hour),
	}

	// Permanent errors should not retry
	if scheduler.ShouldRetry(msg, ErrorPermanent) {
		t.Error("Should not retry permanent errors")
	}
}

func TestRetryScheduler_ShouldRetry_Temporary(t *testing.T) {
	cfg := &config.DeliveryConfig{
		InitialRetryDelaySeconds:  300,
		MaxRetryDelaySeconds:      43200,
		BackoffMultiplier:         2.0,
		GreylistRetryDelaySeconds: 120,
	}

	scheduler := NewRetryScheduler(cfg)

	msg := &queue.QueuedMessage{
		MessageID: "test",
		ExpiresAt: time.Now().Add(24 * time.Hour),
	}

	// Temporary errors should retry if not expired
	if !scheduler.ShouldRetry(msg, ErrorTemporary) {
		t.Error("Should retry temporary errors")
	}
}

func TestRetryScheduler_ShouldRetry_Expired(t *testing.T) {
	cfg := &config.DeliveryConfig{
		InitialRetryDelaySeconds:  300,
		MaxRetryDelaySeconds:      43200,
		BackoffMultiplier:         2.0,
		GreylistRetryDelaySeconds: 120,
	}

	scheduler := NewRetryScheduler(cfg)

	// Expired message
	msg := &queue.QueuedMessage{
		MessageID: "test",
		ExpiresAt: time.Now().Add(-1 * time.Hour), // Expired 1 hour ago
	}

	// Should not retry expired messages
	if scheduler.ShouldRetry(msg, ErrorTemporary) {
		t.Error("Should not retry expired messages")
	}
}

func TestRetryScheduler_GetNextRetryTime(t *testing.T) {
	cfg := &config.DeliveryConfig{
		InitialRetryDelaySeconds:  300,
		MaxRetryDelaySeconds:      43200,
		BackoffMultiplier:         2.0,
		GreylistRetryDelaySeconds: 120,
	}

	scheduler := NewRetryScheduler(cfg)

	now := time.Now()
	nextRetry := scheduler.GetNextRetryTime(1, ErrorTemporary)

	// Should be ~5 minutes from now
	expectedDelay := 300 * time.Second
	actualDelay := nextRetry.Sub(now)

	if actualDelay < expectedDelay-time.Second || actualDelay > expectedDelay+time.Second {
		t.Errorf("Expected next retry ~%v from now, got %v", expectedDelay, actualDelay)
	}
}

func TestRetryScheduler_GetNextRetryTime_Permanent(t *testing.T) {
	cfg := &config.DeliveryConfig{
		InitialRetryDelaySeconds:  300,
		MaxRetryDelaySeconds:      43200,
		BackoffMultiplier:         2.0,
		GreylistRetryDelaySeconds: 120,
	}

	scheduler := NewRetryScheduler(cfg)

	nextRetry := scheduler.GetNextRetryTime(1, ErrorPermanent)

	// Should return zero time for permanent errors
	if !nextRetry.IsZero() {
		t.Errorf("Expected zero time for permanent error, got %v", nextRetry)
	}
}

func TestRetryScheduler_GetRetrySchedule(t *testing.T) {
	cfg := &config.DeliveryConfig{
		InitialRetryDelaySeconds:  300,
		MaxRetryDelaySeconds:      43200,
		BackoffMultiplier:         2.0,
		GreylistRetryDelaySeconds: 120,
	}

	scheduler := NewRetryScheduler(cfg)

	schedule := scheduler.GetRetrySchedule(5)

	if len(schedule) != 5 {
		t.Errorf("Expected schedule length 5, got %d", len(schedule))
	}

	// Verify exponential growth
	for i := 1; i < len(schedule); i++ {
		if schedule[i] <= schedule[i-1] {
			t.Errorf("Schedule should be increasing: attempt %d (%v) should be > attempt %d (%v)",
				i+1, schedule[i], i, schedule[i-1])
		}
	}
}

func TestIsExpired(t *testing.T) {
	// Not expired
	msg := &queue.QueuedMessage{
		ExpiresAt: time.Now().Add(1 * time.Hour),
	}

	if IsExpired(msg) {
		t.Error("Message should not be expired")
	}

	// Expired
	expiredMsg := &queue.QueuedMessage{
		ExpiresAt: time.Now().Add(-1 * time.Hour),
	}

	if !IsExpired(expiredMsg) {
		t.Error("Message should be expired")
	}
}

func TestTimeUntilExpiration(t *testing.T) {
	// 1 hour remaining
	msg := &queue.QueuedMessage{
		ExpiresAt: time.Now().Add(1 * time.Hour),
	}

	remaining := TimeUntilExpiration(msg)
	if remaining < 59*time.Minute || remaining > 61*time.Minute {
		t.Errorf("Expected ~1 hour remaining, got %v", remaining)
	}

	// Already expired
	expiredMsg := &queue.QueuedMessage{
		ExpiresAt: time.Now().Add(-1 * time.Hour),
	}

	remainingExpired := TimeUntilExpiration(expiredMsg)
	if remainingExpired != 0 {
		t.Errorf("Expected 0 for expired message, got %v", remainingExpired)
	}
}

func TestCalculateExpiresAt(t *testing.T) {
	createdAt := time.Now()
	maxAgeHours := 48

	expiresAt := CalculateExpiresAt(createdAt, maxAgeHours)

	expectedExpiry := createdAt.Add(48 * time.Hour)
	if expiresAt.Sub(expectedExpiry) > time.Second {
		t.Errorf("Expected expires_at ~%v, got %v", expectedExpiry, expiresAt)
	}
}

func TestRetryScheduler_NetworkError(t *testing.T) {
	cfg := &config.DeliveryConfig{
		InitialRetryDelaySeconds:  300,
		MaxRetryDelaySeconds:      43200,
		BackoffMultiplier:         2.0,
		GreylistRetryDelaySeconds: 120,
	}

	scheduler := NewRetryScheduler(cfg)

	// Network errors should use exponential backoff like temporary
	delay := scheduler.CalculateNextRetry(1, ErrorNetwork)
	expected := 300 * time.Second

	if delay != expected {
		t.Errorf("Expected network error delay %v, got %v", expected, delay)
	}
}

func TestRetryScheduler_ZeroAttempt(t *testing.T) {
	cfg := &config.DeliveryConfig{
		InitialRetryDelaySeconds:  300,
		MaxRetryDelaySeconds:      43200,
		BackoffMultiplier:         2.0,
		GreylistRetryDelaySeconds: 120,
	}

	scheduler := NewRetryScheduler(cfg)

	// Attempt 0 should be treated as attempt 1
	delay := scheduler.CalculateNextRetry(0, ErrorTemporary)
	expected := 300 * time.Second

	if delay != expected {
		t.Errorf("Attempt 0 should use attempt 1 delay: expected %v, got %v", expected, delay)
	}
}

func TestRetryScheduler_CustomBackoffMultiplier(t *testing.T) {
	cfg := &config.DeliveryConfig{
		InitialRetryDelaySeconds:  60,   // 1 min
		MaxRetryDelaySeconds:      3600, // 1 hour
		BackoffMultiplier:         3.0,  // Triple each time
		GreylistRetryDelaySeconds: 120,
	}

	scheduler := NewRetryScheduler(cfg)

	tests := []struct {
		attempt  int
		expected time.Duration
	}{
		{1, 60 * time.Second},   // 1 min
		{2, 180 * time.Second},  // 3 min (60 * 3)
		{3, 540 * time.Second},  // 9 min (60 * 9)
		{4, 1620 * time.Second}, // 27 min (60 * 27)
	}

	for _, tt := range tests {
		delay := scheduler.CalculateNextRetry(tt.attempt, ErrorTemporary)
		if delay != tt.expected {
			t.Errorf("Attempt %d with 3x multiplier: expected %v, got %v", tt.attempt, tt.expected, delay)
		}
	}
}
