package callback

import (
	"sync"
	"time"

	"go.uber.org/zap"
)

// CallbackCircuitState represents the state of the callback circuit breaker.
//
// The circuit breaker uses a state machine to prevent cascading failures when
// the webhook endpoint becomes unreachable.
type CallbackCircuitState int

const (
	// CallbackCircuitClosed indicates normal operation with callbacks allowed.
	CallbackCircuitClosed CallbackCircuitState = iota

	// CallbackCircuitOpen indicates the circuit is open due to consecutive failures.
	// Callbacks are postponed until the timeout expires.
	CallbackCircuitOpen

	// CallbackCircuitHalfOpen indicates the circuit is testing recovery.
	// A limited number of callbacks are allowed to probe the webhook endpoint.
	CallbackCircuitHalfOpen
)

// CallbackCircuitBreaker implements the circuit breaker pattern for webhook callbacks.
//
// The circuit breaker protects the callback system from overwhelming an unreachable
// or slow webhook endpoint. When the endpoint fails repeatedly, the circuit opens
// and postpones callbacks until the endpoint recovers.
//
// State transitions:
//   - Closed → Open: After N consecutive failures
//   - Open → Half-Open: After timeout expires
//   - Half-Open → Closed: After M consecutive successes
//   - Half-Open → Open: After any failure
//
// The circuit breaker is thread-safe and can be called concurrently by multiple
// callback processors.
type CallbackCircuitBreaker struct {
	mu sync.RWMutex

	// Configuration
	failureThreshold int           // Consecutive failures before opening
	successThreshold int           // Consecutive successes in half-open to close
	openTimeout      time.Duration // Time to wait before half-open
	logger           *zap.Logger

	// State
	state                CallbackCircuitState
	consecutiveFailures  int
	consecutiveSuccesses int
	lastFailureTime      time.Time
	lastStateChange      time.Time
}

// NewCallbackCircuitBreaker creates a new circuit breaker for callbacks.
//
// Parameters:
//   - failureThreshold: Number of consecutive failures before opening (typical: 3-5)
//   - successThreshold: Number of consecutive successes in half-open before closing (typical: 2)
//   - openTimeout: Duration to wait in open state before trying half-open (typical: 1-5 minutes)
//   - logger: Structured logger for state transition events
//
// Returns:
//
//	A new circuit breaker initialized in the closed state.
func NewCallbackCircuitBreaker(failureThreshold, successThreshold int, openTimeout time.Duration, logger *zap.Logger) *CallbackCircuitBreaker {
	return &CallbackCircuitBreaker{
		failureThreshold: failureThreshold,
		successThreshold: successThreshold,
		openTimeout:      openTimeout,
		logger:           logger,
		state:            CallbackCircuitClosed,
		lastStateChange:  time.Now(),
	}
}

// CanAttempt checks if a callback attempt is currently allowed.
//
// The method checks the current circuit state:
//   - Closed: Always allows attempts
//   - Open: Checks if timeout has elapsed; if so, transitions to half-open and allows
//   - Half-Open: Allows attempts to probe recovery
//
// Returns:
//   - true if the callback should be sent
//   - false if the callback should be postponed (circuit is open)
//
// This method is thread-safe and can be called concurrently.
func (cb *CallbackCircuitBreaker) CanAttempt() bool {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	switch cb.state {
	case CallbackCircuitClosed:
		return true

	case CallbackCircuitOpen:
		// Check if enough time has passed to try half-open
		if time.Since(cb.lastFailureTime) >= cb.openTimeout {
			cb.consecutiveSuccesses = 0
			cb.setState(CallbackCircuitHalfOpen)
			cb.logger.Info("callback circuit breaker transitioning to half-open",
				zap.Duration("open_duration", time.Since(cb.lastFailureTime)))
			return true
		}
		return false

	case CallbackCircuitHalfOpen:
		return true

	default:
		return false
	}
}

// RecordSuccess records a successful callback delivery.
//
// This method should be called after a callback is successfully sent (2xx HTTP response).
// It resets the failure counter and, if in half-open state, counts towards the success
// threshold needed to close the circuit.
//
// State transitions:
//   - Closed: No change (remains closed)
//   - Half-Open: Increment success count; close circuit if threshold reached
//
// This method is thread-safe and can be called concurrently.
func (cb *CallbackCircuitBreaker) RecordSuccess() {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	cb.consecutiveFailures = 0

	switch cb.state {
	case CallbackCircuitHalfOpen:
		cb.consecutiveSuccesses++
		if cb.consecutiveSuccesses >= cb.successThreshold {
			cb.consecutiveSuccesses = 0
			cb.setState(CallbackCircuitClosed)
			cb.logger.Info("callback circuit breaker closed after recovery")
		}

	case CallbackCircuitClosed:
		// Already closed, nothing to do
	}
}

// RecordFailure records a failed callback (network/timeout errors only).
//
// This method should be called after a callback fails due to infrastructure issues
// (network errors, DNS failures, timeouts). HTTP 5xx errors from the webhook endpoint
// should NOT be recorded as failures, as they indicate application errors rather than
// infrastructure problems.
//
// State transitions:
//   - Closed: Increment failure count; open circuit if threshold reached
//   - Half-Open: Immediately reopen circuit on any failure
//
// This method is thread-safe and can be called concurrently.
func (cb *CallbackCircuitBreaker) RecordFailure() {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	cb.consecutiveSuccesses = 0
	cb.consecutiveFailures++
	cb.lastFailureTime = time.Now()

	switch cb.state {
	case CallbackCircuitClosed:
		if cb.consecutiveFailures >= cb.failureThreshold {
			cb.setState(CallbackCircuitOpen)
			cb.logger.Error("callback circuit breaker opened due to consecutive network failures",
				zap.Int("failures", cb.consecutiveFailures),
				zap.Int("threshold", cb.failureThreshold))
		}

	case CallbackCircuitHalfOpen:
		// Single failure in half-open immediately reopens
		cb.consecutiveFailures = 1
		cb.setState(CallbackCircuitOpen)
		cb.logger.Warn("callback circuit breaker reopened after half-open failure")
	}
}

// GetState returns the current circuit breaker state.
//
// Returns:
//
//	The current state (CallbackCircuitClosed, CallbackCircuitOpen, or CallbackCircuitHalfOpen).
//
// This method is thread-safe and can be called concurrently.
func (cb *CallbackCircuitBreaker) GetState() CallbackCircuitState {
	cb.mu.RLock()
	defer cb.mu.RUnlock()
	return cb.state
}

// GetStats returns circuit breaker statistics for monitoring and debugging.
//
// Returns:
//
//	A map containing:
//	  - state: Current state as string ("closed", "open", "half-open")
//	  - consecutive_failures: Current failure count
//	  - consecutive_successes: Current success count (in half-open state)
//	  - last_failure_time: Timestamp of most recent failure
//	  - last_state_change: Timestamp of most recent state transition
//
// This method is thread-safe and can be called concurrently.
func (cb *CallbackCircuitBreaker) GetStats() map[string]interface{} {
	cb.mu.RLock()
	defer cb.mu.RUnlock()

	stateStr := "closed"
	switch cb.state {
	case CallbackCircuitOpen:
		stateStr = "open"
	case CallbackCircuitHalfOpen:
		stateStr = "half-open"
	}

	return map[string]interface{}{
		"state":                 stateStr,
		"consecutive_failures":  cb.consecutiveFailures,
		"consecutive_successes": cb.consecutiveSuccesses,
		"last_failure_time":     cb.lastFailureTime,
		"last_state_change":     cb.lastStateChange,
	}
}

// setState transitions the circuit breaker to a new state.
//
// This internal method updates the state and records the transition timestamp.
// It is idempotent - transitioning to the current state has no effect.
func (cb *CallbackCircuitBreaker) setState(newState CallbackCircuitState) {
	if cb.state == newState {
		return
	}

	cb.state = newState
	cb.lastStateChange = time.Now()
}
