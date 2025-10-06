package callback

import (
	"sync"
	"time"

	"go.uber.org/zap"
)

// CallbackCircuitState represents the state of the callback circuit breaker
type CallbackCircuitState int

const (
	CallbackCircuitClosed   CallbackCircuitState = iota // Normal operation
	CallbackCircuitOpen                                 // Rejecting callbacks
	CallbackCircuitHalfOpen                             // Testing if webhook recovered
)

// CallbackCircuitBreaker implements circuit breaker pattern for webhook callbacks
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

// NewCallbackCircuitBreaker creates a new circuit breaker for callbacks
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

// CanAttempt checks if a callback attempt is allowed
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

// RecordSuccess records a successful callback
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

// RecordFailure records a failed callback (network/timeout errors only)
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

// GetState returns the current circuit state
func (cb *CallbackCircuitBreaker) GetState() CallbackCircuitState {
	cb.mu.RLock()
	defer cb.mu.RUnlock()
	return cb.state
}

// GetStats returns circuit breaker statistics
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

// setState transitions to a new state
func (cb *CallbackCircuitBreaker) setState(newState CallbackCircuitState) {
	if cb.state == newState {
		return
	}

	cb.state = newState
	cb.lastStateChange = time.Now()
}
