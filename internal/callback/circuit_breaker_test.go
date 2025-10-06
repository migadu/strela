package callback

import (
	"testing"
	"time"

	"go.uber.org/zap"
)

func TestCallbackCircuitBreakerBasicOperation(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	cb := NewCallbackCircuitBreaker(3, 2, 1*time.Second, logger)

	// Initially should be closed
	if !cb.CanAttempt() {
		t.Error("Circuit should be closed initially")
	}

	// Record 2 failures - should still be closed
	cb.RecordFailure()
	cb.RecordFailure()
	if !cb.CanAttempt() {
		t.Error("Circuit should still be closed after 2 failures (threshold is 3)")
	}

	// 3rd failure should open the circuit
	cb.RecordFailure()
	if cb.CanAttempt() {
		t.Error("Circuit should be open after 3 failures")
	}

	state := cb.GetState()
	if state != CallbackCircuitOpen {
		t.Errorf("Expected state Open, got %v", state)
	}
}

func TestCallbackCircuitBreakerRecovery(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	cb := NewCallbackCircuitBreaker(2, 2, 100*time.Millisecond, logger)

	// Open the circuit
	cb.RecordFailure()
	cb.RecordFailure()
	if cb.GetState() != CallbackCircuitOpen {
		t.Error("Circuit should be open")
	}

	// Should not allow attempts immediately
	if cb.CanAttempt() {
		t.Error("Circuit should not allow attempts when open")
	}

	// Wait for timeout
	time.Sleep(150 * time.Millisecond)

	// Should transition to half-open
	if !cb.CanAttempt() {
		t.Error("Circuit should allow attempts after timeout (half-open)")
	}

	if cb.GetState() != CallbackCircuitHalfOpen {
		t.Error("Circuit should be in half-open state")
	}

	// One success in half-open (need 2 to close)
	cb.RecordSuccess()
	if cb.GetState() != CallbackCircuitHalfOpen {
		t.Error("Circuit should still be half-open after 1 success")
	}

	// Second success should close the circuit
	cb.RecordSuccess()
	if cb.GetState() != CallbackCircuitClosed {
		t.Error("Circuit should be closed after 2 successes")
	}
}

func TestCallbackCircuitBreakerHalfOpenFailure(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	cb := NewCallbackCircuitBreaker(2, 2, 50*time.Millisecond, logger)

	// Open the circuit
	cb.RecordFailure()
	cb.RecordFailure()

	// Wait for half-open
	time.Sleep(100 * time.Millisecond)
	cb.CanAttempt() // Transitions to half-open

	if cb.GetState() != CallbackCircuitHalfOpen {
		t.Error("Circuit should be half-open")
	}

	// Failure in half-open should immediately reopen
	cb.RecordFailure()
	if cb.GetState() != CallbackCircuitOpen {
		t.Error("Circuit should be open after failure in half-open")
	}

	// Should not allow attempts
	if cb.CanAttempt() {
		t.Error("Circuit should not allow attempts after reopening")
	}
}

func TestCallbackCircuitBreakerSuccessResetsFailures(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	cb := NewCallbackCircuitBreaker(3, 2, 1*time.Second, logger)

	// Record 2 failures
	cb.RecordFailure()
	cb.RecordFailure()

	// Success should reset consecutive failures
	cb.RecordSuccess()

	// Now we need 3 more failures to open
	cb.RecordFailure()
	cb.RecordFailure()
	if !cb.CanAttempt() {
		t.Error("Circuit should still be closed")
	}

	cb.RecordFailure()
	if cb.CanAttempt() {
		t.Error("Circuit should be open now")
	}
}

func TestCallbackCircuitBreakerGetStats(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	cb := NewCallbackCircuitBreaker(3, 2, 1*time.Second, logger)

	stats := cb.GetStats()
	if stats["state"] != "closed" {
		t.Errorf("Expected state 'closed', got %v", stats["state"])
	}

	if stats["consecutive_failures"] != 0 {
		t.Errorf("Expected 0 consecutive failures, got %v", stats["consecutive_failures"])
	}

	// Record failures and check stats
	cb.RecordFailure()
	cb.RecordFailure()
	cb.RecordFailure()

	stats = cb.GetStats()
	if stats["state"] != "open" {
		t.Errorf("Expected state 'open', got %v", stats["state"])
	}

	if stats["consecutive_failures"] != 3 {
		t.Errorf("Expected 3 consecutive failures, got %v", stats["consecutive_failures"])
	}
}

func TestIsCallbackNetworkError(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		expected bool
	}{
		{
			name:     "nil error",
			err:      nil,
			expected: false,
		},
		{
			name:     "connection refused",
			err:      &testError{"connection refused"},
			expected: true,
		},
		{
			name:     "timeout error",
			err:      &testError{"context deadline exceeded"},
			expected: true,
		},
		{
			name:     "dns error",
			err:      &testError{"no such host"},
			expected: true,
		},
		{
			name:     "HTTP 500 error",
			err:      &testError{"callback returned status 500"},
			expected: false,
		},
		{
			name:     "HTTP 503 error",
			err:      &testError{"callback returned status 503"},
			expected: false,
		},
		{
			name:     "i/o timeout",
			err:      &testError{"i/o timeout"},
			expected: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := isCallbackNetworkError(tt.err)
			if result != tt.expected {
				t.Errorf("isCallbackNetworkError(%v) = %v, expected %v", tt.err, result, tt.expected)
			}
		})
	}
}

// testError is a simple error implementation for testing
type testError struct {
	msg string
}

func (e *testError) Error() string {
	return e.msg
}
