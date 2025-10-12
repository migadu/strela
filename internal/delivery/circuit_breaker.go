package delivery

import (
	"log/slog"
	"sync"
	"time"
)

// CircuitState represents the state of the circuit breaker.
type CircuitState int

const (
	// CircuitClosed indicates normal operation, delivery attempts are allowed.
	CircuitClosed CircuitState = iota
	// CircuitOpen indicates the circuit is open due to consecutive failures, rejecting new requests.
	CircuitOpen
	// CircuitHalfOpen indicates the circuit is testing recovery with limited requests.
	CircuitHalfOpen
)

// CircuitBreakerMetrics defines the interface for recording circuit breaker metrics
// to Prometheus or other monitoring systems.
type CircuitBreakerMetrics interface {
	SetCircuitBreakerState(state int)
	RecordCircuitBreakerTransition(fromState, toState string)
}

// CircuitBreaker implements the circuit breaker pattern to prevent accepting messages during
// delivery failures. It tracks consecutive local errors (network issues, DNS failures) and
// opens after a configured threshold. Remote SMTP errors (5xx responses from recipient servers)
// do not trigger the circuit breaker, as they are not our infrastructure issues. When open,
// the HTTP API returns 503 Service Unavailable. After a timeout period, the circuit enters
// half-open state to test recovery.
type CircuitBreaker struct {
	mu sync.RWMutex

	// Configuration
	failureThreshold int           // Consecutive failures before opening
	successThreshold int           // Consecutive successes in half-open to close
	openTimeout      time.Duration // Time to wait before half-open
	logger           *slog.Logger
	metrics          CircuitBreakerMetrics

	// State
	state                CircuitState
	consecutiveFailures  int
	consecutiveSuccesses int
	lastFailureTime      time.Time
	lastStateChange      time.Time
}

// NewCircuitBreaker creates a new circuit breaker with the specified thresholds and timeout.
// The failureThreshold defines how many consecutive local failures trigger opening the circuit.
// The successThreshold defines how many consecutive successes in half-open state close the circuit.
// The openTimeout defines how long to wait before entering half-open state.
func NewCircuitBreaker(failureThreshold, successThreshold int, openTimeout time.Duration, logger *slog.Logger) *CircuitBreaker {
	return &CircuitBreaker{
		failureThreshold: failureThreshold,
		successThreshold: successThreshold,
		openTimeout:      openTimeout,
		logger:           logger,
		state:            CircuitClosed,
		lastStateChange:  time.Now(),
	}
}

// CanAttempt checks if a delivery attempt is allowed based on the current circuit state.
// Returns true if the circuit is closed or half-open. Returns false if the circuit is open
// and the timeout has not elapsed. Automatically transitions to half-open if the timeout
// has elapsed.
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
				"open_duration", time.Since(cb.lastFailureTime))
			return true
		}
		return false

	case CircuitHalfOpen:
		return true

	default:
		return false
	}
}

// RecordSuccess records a successful delivery, resetting the failure counter.
// In half-open state, consecutive successes are tracked, and the circuit closes
// after reaching the success threshold.
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

// RecordFailure records a failed delivery attempt. Only local errors (network issues,
// DNS failures, connection timeouts) increment the failure counter. Remote SMTP errors
// (5xx from recipient servers) are ignored as they are not infrastructure issues.
// In closed state, the circuit opens after reaching the failure threshold. In half-open
// state, a single failure immediately reopens the circuit.
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
				"failures", cb.consecutiveFailures,
				"threshold", cb.failureThreshold)
		}

	case CircuitHalfOpen:
		// Single failure in half-open immediately reopens
		cb.consecutiveFailures = 1
		cb.setState(CircuitOpen)
		cb.logger.Warn("circuit breaker reopened after half-open failure")
	}
}

// GetState returns the current circuit state (closed, open, or half-open).
// This method is thread-safe.
func (cb *CircuitBreaker) GetState() CircuitState {
	cb.mu.RLock()
	defer cb.mu.RUnlock()
	return cb.state
}

// GetStats returns circuit breaker statistics including current state, consecutive
// failure and success counts, last failure time, and last state change time.
// This is useful for health checks and monitoring endpoints.
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
		"state":                 stateStr,
		"consecutive_failures":  cb.consecutiveFailures,
		"consecutive_successes": cb.consecutiveSuccesses,
		"last_failure_time":     cb.lastFailureTime,
		"last_state_change":     cb.lastStateChange,
	}
}

// SetMetrics sets the metrics recorder for the circuit breaker, enabling Prometheus
// metrics for circuit state and state transitions. It also sets the initial state metric.
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

// IsLocalError determines if an error is from our local network or configuration issues.
// Local errors include network failures (connection refused, timeouts, DNS failures).
// Remote SMTP errors (temporary/permanent from recipient servers), throttled errors,
// and reputation errors are not considered local. Only local errors should trigger
// circuit breaker opening, as they indicate infrastructure problems on our side.
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

	// Reputation errors are external, not local
	if err.Category == ErrorReputation {
		return false
	}

	// Temporary and permanent SMTP errors are remote, not local
	return false
}
