package delivery

import (
	"math"
	"time"

	"fune/internal/config"
	"fune/internal/queue"
)

// RetryScheduler calculates retry delays with exponential backoff
type RetryScheduler struct {
	config *config.OutboundConfig
}

// NewRetryScheduler creates a new retry scheduler
func NewRetryScheduler(cfg *config.OutboundConfig) *RetryScheduler {
	return &RetryScheduler{config: cfg}
}

// CalculateNextRetry determines when to retry based on attempt number and error category
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

// ShouldRetry determines if a message should be retried
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

// GetNextRetryTime calculates the absolute next retry time
func (r *RetryScheduler) GetNextRetryTime(attemptNumber int, category ErrorCategory) time.Time {
	delay := r.CalculateNextRetry(attemptNumber, category)
	if delay == 0 {
		return time.Time{} // Zero time indicates no retry
	}
	return time.Now().Add(delay)
}

// GetRetrySchedule returns the full retry schedule for documentation/testing
func (r *RetryScheduler) GetRetrySchedule(maxAttempts int) []time.Duration {
	schedule := make([]time.Duration, maxAttempts)
	for i := 0; i < maxAttempts; i++ {
		schedule[i] = r.calculateExponentialBackoff(i + 1)
	}
	return schedule
}

// IsExpired checks if a message has exceeded its lifetime
func IsExpired(msg *queue.QueuedMessage) bool {
	return time.Now().After(msg.ExpiresAt)
}

// TimeUntilExpiration returns remaining time before expiration
func TimeUntilExpiration(msg *queue.QueuedMessage) time.Duration {
	remaining := time.Until(msg.ExpiresAt)
	if remaining < 0 {
		return 0
	}
	return remaining
}

// CalculateExpiresAt calculates expiration time from creation
func CalculateExpiresAt(createdAt time.Time, maxAgeHours int) time.Time {
	return createdAt.Add(time.Duration(maxAgeHours) * time.Hour)
}
