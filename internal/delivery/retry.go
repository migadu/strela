package delivery

import (
	"math"
	"time"

	"fune/internal/config"
	"fune/internal/queue"
)

// RetryScheduler calculates retry delays with exponential backoff and special handling
// for greylisting. It implements a configurable backoff strategy with a maximum delay cap
// to prevent excessively long retry intervals. The scheduler distinguishes between
// different error categories and applies appropriate retry logic.
type RetryScheduler struct {
	config *config.OutboundConfig
}

// NewRetryScheduler creates a new retry scheduler with the specified configuration.
// The config defines initial delay, backoff multiplier, max delay, and greylist retry delay.
func NewRetryScheduler(cfg *config.OutboundConfig) *RetryScheduler {
	return &RetryScheduler{config: cfg}
}

// CalculateNextRetry determines the retry delay based on attempt number and error category.
// Permanent errors return 0 (no retry). Greylist errors use a fast retry (default 2 minutes).
// Temporary and network errors use exponential backoff (e.g., 5min → 10min → 20min → ... → 12hr max).
// The attempt number should be 1-indexed (first attempt = 1).
func (r *RetryScheduler) CalculateNextRetry(attemptNumber int, category ErrorCategory) time.Duration {
	switch category {
	case ErrorPermanent:
		// No retry for permanent errors
		return 0

	case ErrorGreylist:
		// Aggressive retry for greylisting (2 minutes)
		return time.Duration(r.config.GreylistRetryDelaySeconds) * time.Second

	case ErrorTemporary, ErrorNetwork:
		// Exponential backoff for temporary/network errors
		return r.calculateExponentialBackoff(attemptNumber)

	default:
		// Default to exponential backoff
		return r.calculateExponentialBackoff(attemptNumber)
	}
}

// calculateExponentialBackoff implements exponential backoff with cap
func (r *RetryScheduler) calculateExponentialBackoff(attemptNumber int) time.Duration {
	if attemptNumber <= 0 {
		attemptNumber = 1
	}

	// Calculate delay: initialDelay * (multiplier ^ (attempt - 1))
	baseDelay := float64(r.config.InitialRetryDelaySeconds)
	multiplier := r.config.BackoffMultiplier
	exponent := float64(attemptNumber - 1)

	delaySeconds := baseDelay * math.Pow(multiplier, exponent)

	// Cap at max delay
	maxDelay := float64(r.config.MaxRetryDelaySeconds)
	if delaySeconds > maxDelay {
		delaySeconds = maxDelay
	}

	return time.Duration(delaySeconds) * time.Second
}

// ShouldRetry determines if a message should be retried based on error category and expiration.
// Permanent errors are never retried. Expired messages (past their max age) are not retried.
// All other retryable error categories (temporary, greylist, network, throttled, reputation)
// are retried until expiration.
func (r *RetryScheduler) ShouldRetry(msg *queue.QueuedMessage, category ErrorCategory) bool {
	// Don't retry permanent errors
	if category == ErrorPermanent {
		return false
	}

	// Check if message has expired
	if time.Now().After(msg.ExpiresAt) {
		return false
	}

	// Retry temporary, greylist, and network errors if not expired
	return IsRetryable(category)
}

// GetNextRetryTime calculates the absolute next retry time (now + delay).
// Returns a zero time if no retry should occur (permanent errors).
func (r *RetryScheduler) GetNextRetryTime(attemptNumber int, category ErrorCategory) time.Time {
	delay := r.CalculateNextRetry(attemptNumber, category)
	if delay == 0 {
		return time.Time{} // Zero time indicates no retry
	}
	return time.Now().Add(delay)
}

// GetRetrySchedule returns the full exponential backoff retry schedule for the given
// number of attempts. This is useful for testing and documentation purposes to see
// the complete backoff progression (e.g., [5m, 10m, 20m, 40m, 1h20m, ...]).
func (r *RetryScheduler) GetRetrySchedule(maxAttempts int) []time.Duration {
	schedule := make([]time.Duration, maxAttempts)
	for i := 0; i < maxAttempts; i++ {
		schedule[i] = r.calculateExponentialBackoff(i + 1)
	}
	return schedule
}

// IsExpired checks if a message has exceeded its maximum age (expires_at).
// Expired messages should not be retried and should be marked with a terminal status.
func IsExpired(msg *queue.QueuedMessage) bool {
	return time.Now().After(msg.ExpiresAt)
}

// TimeUntilExpiration returns the remaining time before a message expires.
// Returns 0 if the message is already expired.
func TimeUntilExpiration(msg *queue.QueuedMessage) time.Duration {
	remaining := time.Until(msg.ExpiresAt)
	if remaining < 0 {
		return 0
	}
	return remaining
}

// CalculateExpiresAt calculates the expiration time from the creation time and max age.
// This is used when initially queueing messages to set the expires_at field.
// Messages past this time will not be retried and will be marked as expired.
func CalculateExpiresAt(createdAt time.Time, maxAgeHours int) time.Time {
	return createdAt.Add(time.Duration(maxAgeHours) * time.Hour)
}
