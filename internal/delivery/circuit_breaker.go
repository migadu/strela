package delivery

import (
	"sync"
	"time"

	"go.uber.org/zap"
)

// CircuitState represents the state of the circuit breaker
type CircuitState int

const (
	CircuitClosed   CircuitState = iota // Normal operation
	CircuitOpen                         // Rejecting requests
	CircuitHalfOpen                     // Testing if service recovered
)

// CircuitBreakerMetrics interface for recording circuit breaker metrics
type CircuitBreakerMetrics interface {
	SetCircuitBreakerState(state int)
	RecordCircuitBreakerTransition(fromState, toState string)
}

// CircuitBreaker implements circuit breaker pattern for delivery failures
type CircuitBreaker struct {
	mu sync.RWMutex

	// Configuration
	failureThreshold int           // Consecutive failures before opening
	successThreshold int           // Consecutive successes in half-open to close
	openTimeout      time.Duration // Time to wait before half-open
	logger           *zap.Logger
	metrics          CircuitBreakerMetrics

	// State
	state             CircuitState
	consecutiveFailures int
	consecutiveSuccesses int
	lastFailureTime   time.Time
	lastStateChange   time.Time
}

// NewCircuitBreaker creates a new circuit breaker
func NewCircuitBreaker(failureThreshold, successThreshold int, openTimeout time.Duration, logger *zap.Logger) *CircuitBreaker {
	return &CircuitBreaker{
		failureThreshold: failureThreshold,
		successThreshold: successThreshold,
		openTimeout:      openTimeout,
		logger:           logger,
		state:            CircuitClosed,
		lastStateChange:  time.Now(),
	}
}

// CanAttempt checks if a delivery attempt is allowed
func (cb *CircuitBreaker) CanAttempt() bool {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	switch cb.state {
	case CircuitClosed:
		return true

	case CircuitOpen:
		// Check if enough time has passed to try half-open
		if time.Since(cb.lastFailureTime) >= cb.openTimeout {
			cb.consecutiveSuccesses = 0
			cb.setState(CircuitHalfOpen)
			cb.logger.Info("circuit breaker transitioning to half-open",
				zap.Duration("open_duration", time.Since(cb.lastFailureTime)))
			return true
		}
		return false

	case CircuitHalfOpen:
		return true

	default:
		return false
	}
}

// RecordSuccess records a successful delivery
func (cb *CircuitBreaker) RecordSuccess() {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	cb.consecutiveFailures = 0

	switch cb.state {
	case CircuitHalfOpen:
		cb.consecutiveSuccesses++
		if cb.consecutiveSuccesses >= cb.successThreshold {
			cb.consecutiveSuccesses = 0
			cb.setState(CircuitClosed)
			cb.logger.Info("circuit breaker closed after recovery")
		}

	case CircuitClosed:
		// Already closed, nothing to do
	}
}

// RecordFailure records a failed delivery
func (cb *CircuitBreaker) RecordFailure(isLocalError bool) {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	// Only count local errors (network issues from our side)
	if !isLocalError {
		return
	}

	cb.consecutiveSuccesses = 0
	cb.consecutiveFailures++
	cb.lastFailureTime = time.Now()

	switch cb.state {
	case CircuitClosed:
		if cb.consecutiveFailures >= cb.failureThreshold {
			cb.setState(CircuitOpen)
			cb.logger.Error("circuit breaker opened due to consecutive failures",
				zap.Int("failures", cb.consecutiveFailures),
				zap.Int("threshold", cb.failureThreshold))
		}

	case CircuitHalfOpen:
		// Single failure in half-open immediately reopens
		cb.consecutiveFailures = 1
		cb.setState(CircuitOpen)
		cb.logger.Warn("circuit breaker reopened after half-open failure")
	}
}

// GetState returns the current circuit state
func (cb *CircuitBreaker) GetState() CircuitState {
	cb.mu.RLock()
	defer cb.mu.RUnlock()
	return cb.state
}

// GetStats returns circuit breaker statistics
func (cb *CircuitBreaker) GetStats() map[string]interface{} {
	cb.mu.RLock()
	defer cb.mu.RUnlock()

	stateStr := "closed"
	switch cb.state {
	case CircuitOpen:
		stateStr = "open"
	case CircuitHalfOpen:
		stateStr = "half-open"
	}

	return map[string]interface{}{
		"state":                stateStr,
		"consecutive_failures": cb.consecutiveFailures,
		"consecutive_successes": cb.consecutiveSuccesses,
		"last_failure_time":    cb.lastFailureTime,
		"last_state_change":    cb.lastStateChange,
	}
}

// SetMetrics sets the metrics recorder for the circuit breaker
func (cb *CircuitBreaker) SetMetrics(metrics CircuitBreakerMetrics) {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	cb.metrics = metrics
	// Set initial state
	if cb.metrics != nil {
		cb.metrics.SetCircuitBreakerState(int(cb.state))
	}
}

// setState transitions to a new state and records metrics
func (cb *CircuitBreaker) setState(newState CircuitState) {
	if cb.state == newState {
		return
	}

	oldState := cb.state
	cb.state = newState
	cb.lastStateChange = time.Now()

	// Record metrics if available
	if cb.metrics != nil {
		cb.metrics.SetCircuitBreakerState(int(newState))

		oldStateStr := stateToString(oldState)
		newStateStr := stateToString(newState)
		cb.metrics.RecordCircuitBreakerTransition(oldStateStr, newStateStr)
	}
}

// stateToString converts CircuitState to string
func stateToString(state CircuitState) string {
	switch state {
	case CircuitClosed:
		return "closed"
	case CircuitOpen:
		return "open"
	case CircuitHalfOpen:
		return "half_open"
	default:
		return "unknown"
	}
}

// IsLocalError determines if an error is from our local network/configuration
func IsLocalError(err *DeliveryError) bool {
	if err == nil {
		return false
	}

	// Network errors (connection refused, timeouts, DNS failures) are local
	if err.Category == ErrorNetwork {
		return true
	}

	// Throttled is our own rate limiting, not a local error
	if err.Category == ErrorThrottled {
		return false
	}

	// Temporary and permanent SMTP errors are remote, not local
	return false
}
