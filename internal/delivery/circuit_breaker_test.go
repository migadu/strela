package delivery

import (
	"testing"
	"time"

	"log/slog"
)

// MockCircuitBreakerMetrics implements CircuitBreakerMetrics for testing
type MockCircuitBreakerMetrics struct {
	states      []int
	transitions []struct{ from, to string }
}

func (m *MockCircuitBreakerMetrics) SetCircuitBreakerState(state int) {
	m.states = append(m.states, state)
}

func (m *MockCircuitBreakerMetrics) RecordCircuitBreakerTransition(fromState, toState string) {
	m.transitions = append(m.transitions, struct{ from, to string }{fromState, toState})
}

func TestCircuitBreaker_InitialState(t *testing.T) {
	logger := slog.Default()
	cb := NewCircuitBreaker(3, 2, 5*time.Second, logger)

	if cb.GetState() != CircuitClosed {
		t.Errorf("expected initial state to be Closed, got %v", cb.GetState())
	}

	if !cb.CanAttempt() {
		t.Error("expected CanAttempt to be true in initial state")
	}
}

func TestCircuitBreaker_OpenOnFailures(t *testing.T) {
	logger := slog.Default()
	cb := NewCircuitBreaker(3, 2, 100*time.Millisecond, logger)

	// Record local errors to trigger circuit breaker
	for i := 0; i < 3; i++ {
		cb.RecordFailure(true)
	}

	if cb.GetState() != CircuitOpen {
		t.Errorf("expected state to be Open after threshold failures, got %v", cb.GetState())
	}

	if cb.CanAttempt() {
		t.Error("expected CanAttempt to be false when circuit is open")
	}
}

func TestCircuitBreaker_IgnoreNonLocalErrors(t *testing.T) {
	logger := slog.Default()
	cb := NewCircuitBreaker(3, 2, 5*time.Second, logger)

	// Record non-local errors (should be ignored)
	for i := 0; i < 5; i++ {
		cb.RecordFailure(false)
	}

	if cb.GetState() != CircuitClosed {
		t.Errorf("expected state to remain Closed for non-local errors, got %v", cb.GetState())
	}

	stats := cb.GetStats()
	if stats["consecutive_failures"].(int) != 0 {
		t.Errorf("expected 0 consecutive failures for non-local errors, got %d", stats["consecutive_failures"])
	}
}

func TestCircuitBreaker_HalfOpenTransition(t *testing.T) {
	logger := slog.Default()
	cb := NewCircuitBreaker(3, 2, 50*time.Millisecond, logger)

	// Open the circuit
	for i := 0; i < 3; i++ {
		cb.RecordFailure(true)
	}

	if cb.GetState() != CircuitOpen {
		t.Fatal("expected circuit to be open")
	}

	// Wait for timeout
	time.Sleep(60 * time.Millisecond)

	// Next CanAttempt should transition to half-open
	if !cb.CanAttempt() {
		t.Error("expected CanAttempt to be true after timeout")
	}

	if cb.GetState() != CircuitHalfOpen {
		t.Errorf("expected state to be HalfOpen after timeout, got %v", cb.GetState())
	}
}

func TestCircuitBreaker_RecoverFromHalfOpen(t *testing.T) {
	logger := slog.Default()
	cb := NewCircuitBreaker(3, 2, 50*time.Millisecond, logger)

	// Open the circuit
	for i := 0; i < 3; i++ {
		cb.RecordFailure(true)
	}

	// Wait and transition to half-open
	time.Sleep(60 * time.Millisecond)
	cb.CanAttempt()

	if cb.GetState() != CircuitHalfOpen {
		t.Fatal("expected circuit to be half-open")
	}

	// Record successful attempts to close circuit
	cb.RecordSuccess()
	if cb.GetState() == CircuitClosed {
		t.Error("expected circuit to remain half-open after 1 success (threshold is 2)")
	}

	cb.RecordSuccess()
	if cb.GetState() != CircuitClosed {
		t.Errorf("expected circuit to close after threshold successes, got %v", cb.GetState())
	}
}

func TestCircuitBreaker_ReopenFromHalfOpen(t *testing.T) {
	logger := slog.Default()
	cb := NewCircuitBreaker(3, 2, 50*time.Millisecond, logger)

	// Open the circuit
	for i := 0; i < 3; i++ {
		cb.RecordFailure(true)
	}

	// Wait and transition to half-open
	time.Sleep(60 * time.Millisecond)
	cb.CanAttempt()

	if cb.GetState() != CircuitHalfOpen {
		t.Fatal("expected circuit to be half-open")
	}

	// Record a failure in half-open state
	cb.RecordFailure(true)

	if cb.GetState() != CircuitOpen {
		t.Errorf("expected circuit to reopen after half-open failure, got %v", cb.GetState())
	}
}

func TestCircuitBreaker_SuccessResetsFailures(t *testing.T) {
	logger := slog.Default()
	cb := NewCircuitBreaker(3, 2, 5*time.Second, logger)

	// Record some failures (not enough to open)
	cb.RecordFailure(true)
	cb.RecordFailure(true)

	stats := cb.GetStats()
	if stats["consecutive_failures"].(int) != 2 {
		t.Errorf("expected 2 consecutive failures, got %d", stats["consecutive_failures"])
	}

	// Record success
	cb.RecordSuccess()

	stats = cb.GetStats()
	if stats["consecutive_failures"].(int) != 0 {
		t.Errorf("expected failures to be reset after success, got %d", stats["consecutive_failures"])
	}

	if cb.GetState() != CircuitClosed {
		t.Errorf("expected circuit to remain closed, got %v", cb.GetState())
	}
}

func TestCircuitBreaker_GetStats(t *testing.T) {
	logger := slog.Default()
	cb := NewCircuitBreaker(3, 2, 5*time.Second, logger)

	// Record some failures
	cb.RecordFailure(true)
	cb.RecordFailure(true)

	stats := cb.GetStats()

	if stats["state"].(string) != "closed" {
		t.Errorf("expected state 'closed', got %s", stats["state"])
	}

	if stats["consecutive_failures"].(int) != 2 {
		t.Errorf("expected 2 consecutive failures, got %d", stats["consecutive_failures"])
	}

	if stats["consecutive_successes"].(int) != 0 {
		t.Errorf("expected 0 consecutive successes, got %d", stats["consecutive_successes"])
	}

	// Open the circuit
	cb.RecordFailure(true)

	stats = cb.GetStats()
	if stats["state"].(string) != "open" {
		t.Errorf("expected state 'open', got %s", stats["state"])
	}
}

func TestCircuitBreaker_SetMetrics(t *testing.T) {
	logger := slog.Default()
	cb := NewCircuitBreaker(3, 2, 5*time.Second, logger)

	mockMetrics := &MockCircuitBreakerMetrics{}
	cb.SetMetrics(mockMetrics)

	// Initial state should be recorded
	if len(mockMetrics.states) != 1 || mockMetrics.states[0] != int(CircuitClosed) {
		t.Errorf("expected initial state to be recorded, got %v", mockMetrics.states)
	}

	// Open the circuit
	for i := 0; i < 3; i++ {
		cb.RecordFailure(true)
	}

	// Should have recorded state transitions
	if len(mockMetrics.states) != 2 {
		t.Errorf("expected 2 states recorded, got %d", len(mockMetrics.states))
	}

	if len(mockMetrics.transitions) != 1 {
		t.Errorf("expected 1 transition recorded, got %d", len(mockMetrics.transitions))
	}

	if mockMetrics.transitions[0].from != "closed" || mockMetrics.transitions[0].to != "open" {
		t.Errorf("expected transition from closed to open, got %v -> %v",
			mockMetrics.transitions[0].from, mockMetrics.transitions[0].to)
	}
}

func TestCircuitBreaker_StateToString(t *testing.T) {
	tests := []struct {
		state    CircuitState
		expected string
	}{
		{CircuitClosed, "closed"},
		{CircuitOpen, "open"},
		{CircuitHalfOpen, "half_open"},
		{CircuitState(99), "unknown"},
	}

	for _, tt := range tests {
		result := stateToString(tt.state)
		if result != tt.expected {
			t.Errorf("stateToString(%v) = %s, expected %s", tt.state, result, tt.expected)
		}
	}
}

func TestIsLocalError(t *testing.T) {
	tests := []struct {
		name     string
		err      *DeliveryError
		expected bool
	}{
		{
			name:     "nil error",
			err:      nil,
			expected: false,
		},
		{
			name:     "network error",
			err:      &DeliveryError{Category: ErrorNetwork},
			expected: true,
		},
		{
			name:     "throttled error",
			err:      &DeliveryError{Category: ErrorThrottled},
			expected: false,
		},
		{
			name:     "reputation error",
			err:      &DeliveryError{Category: ErrorReputation},
			expected: false,
		},
		{
			name:     "temporary error",
			err:      &DeliveryError{Category: ErrorTemporary},
			expected: false,
		},
		{
			name:     "permanent error",
			err:      &DeliveryError{Category: ErrorPermanent},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := IsLocalError(tt.err)
			if result != tt.expected {
				t.Errorf("IsLocalError() = %v, expected %v", result, tt.expected)
			}
		})
	}
}

func TestCircuitBreaker_ConcurrentAccess(t *testing.T) {
	logger := slog.Default()
	cb := NewCircuitBreaker(10, 5, 100*time.Millisecond, logger)

	// Run concurrent operations
	done := make(chan bool)
	for i := 0; i < 10; i++ {
		go func() {
			for j := 0; j < 100; j++ {
				cb.CanAttempt()
				cb.RecordSuccess()
				cb.RecordFailure(true)
				cb.GetState()
				cb.GetStats()
			}
			done <- true
		}()
	}

	// Wait for all goroutines
	for i := 0; i < 10; i++ {
		<-done
	}

	// Should not panic and should be in a valid state
	state := cb.GetState()
	if state != CircuitClosed && state != CircuitOpen && state != CircuitHalfOpen {
		t.Errorf("invalid state after concurrent access: %v", state)
	}
}

func TestCircuitBreaker_MultipleStateTransitions(t *testing.T) {
	logger := slog.Default()
	cb := NewCircuitBreaker(2, 2, 50*time.Millisecond, logger)

	mockMetrics := &MockCircuitBreakerMetrics{}
	cb.SetMetrics(mockMetrics)

	// Initial: Closed
	if cb.GetState() != CircuitClosed {
		t.Fatal("expected initial state to be Closed")
	}

	// Closed -> Open
	cb.RecordFailure(true)
	cb.RecordFailure(true)

	if cb.GetState() != CircuitOpen {
		t.Fatal("expected state to be Open")
	}

	// Wait for timeout
	time.Sleep(60 * time.Millisecond)

	// Open -> HalfOpen
	cb.CanAttempt()

	if cb.GetState() != CircuitHalfOpen {
		t.Fatal("expected state to be HalfOpen")
	}

	// HalfOpen -> Closed
	cb.RecordSuccess()
	cb.RecordSuccess()

	if cb.GetState() != CircuitClosed {
		t.Fatal("expected state to be Closed")
	}

	// Verify all transitions were recorded
	expectedTransitions := []struct{ from, to string }{
		{"closed", "open"},
		{"open", "half_open"},
		{"half_open", "closed"},
	}

	if len(mockMetrics.transitions) != len(expectedTransitions) {
		t.Fatalf("expected %d transitions, got %d", len(expectedTransitions), len(mockMetrics.transitions))
	}

	for i, expected := range expectedTransitions {
		actual := mockMetrics.transitions[i]
		if actual.from != expected.from || actual.to != expected.to {
			t.Errorf("transition %d: expected %s -> %s, got %s -> %s",
				i, expected.from, expected.to, actual.from, actual.to)
		}
	}
}
