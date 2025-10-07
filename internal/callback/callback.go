package callback

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"fune/internal/config"
	"fune/internal/delivery"
	"fune/internal/queue"
	"fune/internal/recovery"

	"go.uber.org/zap"
)

// CallbackMetrics defines the interface for recording callback processing metrics.
//
// Implementations should track success rates, failure rates, and latency for different
// event types to enable monitoring and alerting on webhook delivery issues.
type CallbackMetrics interface {
	// RecordCallbackAttempt records the outcome of a callback attempt.
	//
	// Parameters:
	//   - outcome: "success" or "failure"
	//   - eventType: the delivery event type ("delivered", "hard_bounce", etc.)
	//   - duration: HTTP request duration in seconds
	RecordCallbackAttempt(outcome, eventType string, duration float64)
}

// CallbackHandler manages webhook callbacks for delivery events.
//
// CallbackHandler processes callbacks asynchronously using a dedicated worker that
// retrieves pending callbacks from the queue, sends HTTP POST requests to the
// configured webhook URL, and handles failures with exponential backoff retry logic.
//
// The handler integrates with a circuit breaker to prevent cascading failures when
// the webhook endpoint becomes unreachable.
type CallbackHandler struct {
	queue          *queue.Queue
	config         *config.CallbacksConfig
	logger         *zap.Logger
	client         *http.Client
	ctx            context.Context
	cancel         context.CancelFunc
	wg             sync.WaitGroup
	metrics        CallbackMetrics
	circuitBreaker *CallbackCircuitBreaker
}

// NewCallbackHandler creates a new callback handler instance.
//
// The handler is initialized with a dedicated HTTP client (with configured timeout),
// and optionally a circuit breaker if enabled in the configuration.
//
// Parameters:
//   - q: Message queue containing the callback queue
//   - cfg: Callback configuration (webhook URL, timeouts, circuit breaker settings)
//   - logger: Structured logger for observability
//
// The returned handler is ready to start but not yet running. Call Start() to begin
// processing callbacks, and SetMetrics() to enable metrics recording.
func NewCallbackHandler(q *queue.Queue, cfg *config.CallbacksConfig, logger *zap.Logger) *CallbackHandler {
	ctx, cancel := context.WithCancel(context.Background())

	var cb *CallbackCircuitBreaker
	if cfg.CircuitBreakerEnabled {
		cb = NewCallbackCircuitBreaker(
			cfg.CircuitBreakerFailureThreshold,
			cfg.CircuitBreakerSuccessThreshold,
			time.Duration(cfg.CircuitBreakerOpenTimeoutSecs)*time.Second,
			logger,
		)
		logger.Info("callback circuit breaker enabled",
			zap.Int("failure_threshold", cfg.CircuitBreakerFailureThreshold),
			zap.Int("success_threshold", cfg.CircuitBreakerSuccessThreshold),
			zap.Int("open_timeout_secs", cfg.CircuitBreakerOpenTimeoutSecs))
	}

	return &CallbackHandler{
		queue:  q,
		config: cfg,
		logger: logger,
		client: &http.Client{
			Timeout: time.Duration(cfg.TimeoutSeconds) * time.Second,
		},
		ctx:            ctx,
		cancel:         cancel,
		circuitBreaker: cb,
	}
}

// Start begins processing the callback queue.
//
// This method spawns a single callback processor goroutine that continuously
// retrieves and sends pending callbacks. The processor uses a hybrid notification
// and polling approach similar to the message delivery workers.
//
// The method returns immediately after spawning the processor goroutine.
func (c *CallbackHandler) Start() {
	c.logger.Info("starting callback processor")

	c.wg.Add(1)
	go c.processCallbacks()
}

// Stop gracefully shuts down the callback processor.
//
// This method signals the processor to stop via context cancellation and waits
// for it to finish processing the current callback. In-flight HTTP requests are
// allowed to complete before the processor exits.
//
// Stop is blocking and will not return until the processor has fully stopped.
func (c *CallbackHandler) Stop() {
	c.logger.Info("stopping callback processor...")
	c.cancel()
	c.wg.Wait()
	c.logger.Info("callback processor stopped")
}

// SetMetrics configures the metrics recorder for callback operations.
//
// This method should be called before Start() to enable metrics recording.
// If not called, callback operations will proceed without metrics.
func (c *CallbackHandler) SetMetrics(metrics CallbackMetrics) {
	c.metrics = metrics
}

// EnqueueDeliveredCallback queues a webhook notification for successful delivery.
//
// This method is called by delivery workers after a message is successfully delivered
// to the recipient MX server (SMTP 2xx response). The callback includes delivery details
// such as MX host, source IP, SMTP response, and delivery duration.
//
// Parameters:
//   - msg: The queued message that was delivered
//   - result: Delivery result containing MX host, source IP, and SMTP response
//
// If enqueueing fails (database error), the error is logged but does not affect
// the delivery status. The callback can be manually retried via admin tools.
func (c *CallbackHandler) EnqueueDeliveredCallback(msg *queue.QueuedMessage, result *delivery.DeliveryResult) {
	payload := DeliveryEventCallback{
		MessageID:    msg.MessageID,
		Event:        "delivered",
		Email:        msg.ToAddr,
		From:         msg.FromAddr,
		Subject:      msg.Subject,
		DeliveredAt:  time.Now().Format(time.RFC3339),
		Attempts:     msg.Attempts + 1,
		SMTPCode:     result.SMTPCode,
		SMTPResponse: result.SMTPResponse,
		FinalMXHost:  result.MXHost,
		SourceIP:     result.SourceIP,
	}

	if err := c.queue.EnqueueCallback(msg.MessageID, "delivered", payload); err != nil {
		c.logger.Error("failed to enqueue delivered callback",
			zap.String("message_id", msg.MessageID),
			zap.Error(err))
	}
}

// EnqueueHardBounceCallback queues a webhook notification for permanent delivery failure.
//
// This method is called by delivery workers when a message receives a permanent failure
// response (5xx SMTP code) from the recipient MX server. Common causes include:
//   - 550: Mailbox unavailable or rejected
//   - 551: User not local
//   - 552: Mailbox full
//   - 554: Transaction failed
//
// The callback includes the SMTP error code, response message, and failure reason.
//
// Parameters:
//   - msg: The queued message that failed permanently
//   - result: Delivery result containing SMTP error details
func (c *CallbackHandler) EnqueueHardBounceCallback(msg *queue.QueuedMessage, result *delivery.DeliveryResult) {
	reason := "permanent_bounce"
	if result.Error != nil {
		reason = result.Error.Message
	}

	payload := DeliveryEventCallback{
		MessageID:    msg.MessageID,
		Event:        "hard_bounce",
		Email:        msg.ToAddr,
		From:         msg.FromAddr,
		Subject:      msg.Subject,
		DeliveredAt:  time.Now().Format(time.RFC3339),
		Attempts:     msg.Attempts + 1,
		SMTPCode:     result.SMTPCode,
		SMTPResponse: result.SMTPResponse,
		FinalMXHost:  result.MXHost,
		SourceIP:     result.SourceIP,
		Reason:       reason,
	}

	if err := c.queue.EnqueueCallback(msg.MessageID, "hard_bounce", payload); err != nil {
		c.logger.Error("failed to enqueue hard bounce callback",
			zap.String("message_id", msg.MessageID),
			zap.Error(err))
	}
}

// EnqueueTempExpiredCallback queues a webhook notification for temporary failure exhaustion.
//
// This method is called when a message exhausts its retry attempts after temporary
// failures (4xx SMTP codes, network errors, DNS failures) and reaches the maximum
// message age without successful delivery. Common scenarios:
//   - Recipient server repeatedly unavailable
//   - Persistent greylisting beyond retry window
//   - DNS resolution consistently failing
//
// Unlike permanent failures, temporary failures were retried multiple times with
// exponential backoff before being abandoned.
//
// Parameters:
//   - msg: The queued message that expired after temporary failures
//   - result: Delivery result from the last attempt (may be nil for cleanup worker calls)
func (c *CallbackHandler) EnqueueTempExpiredCallback(msg *queue.QueuedMessage, result *delivery.DeliveryResult) {
	reason := "temporary_failure_exhausted"
	var smtpCode int
	var smtpResponse string

	if result != nil {
		smtpCode = result.SMTPCode
		smtpResponse = result.SMTPResponse
		if result.Error != nil {
			reason = result.Error.Message
		}
	}

	payload := DeliveryEventCallback{
		MessageID:    msg.MessageID,
		Event:        "temp_expired",
		Email:        msg.ToAddr,
		From:         msg.FromAddr,
		Subject:      msg.Subject,
		DeliveredAt:  time.Now().Format(time.RFC3339),
		Attempts:     msg.Attempts + 1,
		SMTPCode:     smtpCode,
		SMTPResponse: smtpResponse,
		Reason:       reason,
	}

	if err := c.queue.EnqueueCallback(msg.MessageID, "temp_expired", payload); err != nil {
		c.logger.Error("failed to enqueue temp expired callback",
			zap.String("message_id", msg.MessageID),
			zap.Error(err))
	}
}

// EnqueueExpiredCallback queues a webhook notification for message expiration.
//
// This method is called when a message exceeds its maximum age (default 48 hours)
// without successful delivery and without being classified as a hard bounce or
// temporary failure exhaustion. This typically occurs when:
//   - Messages remain stuck in "queued" status past expiration (detected by cleanup worker)
//   - System backlogs prevent timely processing
//
// The callback includes the total number of attempts made before expiration.
//
// Parameters:
//   - msg: The queued message that expired
//   - result: Delivery result from the last attempt (may be nil for cleanup worker calls)
func (c *CallbackHandler) EnqueueExpiredCallback(msg *queue.QueuedMessage, result *delivery.DeliveryResult) {
	payload := DeliveryEventCallback{
		MessageID:   msg.MessageID,
		Event:       "expired",
		Email:       msg.ToAddr,
		From:        msg.FromAddr,
		Subject:     msg.Subject,
		DeliveredAt: time.Now().Format(time.RFC3339),
		Attempts:    msg.Attempts,
		Reason:      "delivery_timeout",
	}

	if err := c.queue.EnqueueCallback(msg.MessageID, "expired", payload); err != nil {
		c.logger.Error("failed to enqueue expired callback",
			zap.String("message_id", msg.MessageID),
			zap.Error(err))
	}
}

// processCallbacks is the main callback processing loop.
//
// This goroutine continuously retrieves and processes pending callbacks using a
// hybrid notification and polling approach:
//   - Instant processing when notified via channel
//   - Periodic polling every 30 seconds as fallback
//   - Graceful shutdown on context cancellation
//
// The loop runs until Stop() is called.
func (c *CallbackHandler) processCallbacks() {
	defer c.wg.Done()
	defer recovery.RecoverPanic(c.logger, "callback processor")

	// Fallback ticker in case notifications are missed
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	notifyCh := c.queue.CallbackNotifyChan()

	c.logger.Info("callback processor started")

	for {
		select {
		case <-c.ctx.Done():
			c.logger.Info("callback processor stopping")
			return

		case <-notifyCh:
			// New callback notification received - process immediately
			c.processBatch()

		case <-ticker.C:
			// Periodic poll as fallback
			c.processBatch()
		}
	}
}

// processBatch retrieves and processes a batch of pending callbacks.
//
// The method retrieves up to BatchSize callbacks from the queue and sends them
// sequentially. If the context is canceled during processing, remaining callbacks
// are left in the queue and will be processed on restart.
func (c *CallbackHandler) processBatch() {
	callbacks, err := c.queue.GetPendingCallbacks(c.config.BatchSize)
	if err != nil {
		c.logger.Error("failed to get pending callbacks",
			zap.Error(err))
		return
	}

	if len(callbacks) == 0 {
		return
	}

	c.logger.Debug("processing callback batch",
		zap.Int("count", len(callbacks)))

	for _, cb := range callbacks {
		// Check if context is canceled before processing
		select {
		case <-c.ctx.Done():
			c.logger.Info("callback processor stopping, remaining callbacks will be processed on restart")
			return
		default:
			c.sendCallback(cb)
		}
	}
}

// sendCallback attempts to send a single callback via HTTP POST.
//
// The method:
//  1. Checks circuit breaker state (skips if open, reschedules after timeout)
//  2. Parses the callback payload from JSON
//  3. Sends HTTP POST request to webhook URL with bearer token auth
//  4. Records success/failure with circuit breaker
//  5. On success: marks callback as complete
//  6. On failure: schedules retry with exponential backoff or abandons if expired
//
// Failed callbacks are retried with exponential backoff up to the maximum callback
// age (default 24 hours). Callbacks exceeding max age are marked complete to prevent
// indefinite retries.
func (c *CallbackHandler) sendCallback(cb queue.PendingCallback) {
	startTime := time.Now()

	// Check circuit breaker before attempting callback
	if c.circuitBreaker != nil && !c.circuitBreaker.CanAttempt() {
		c.logger.Warn("callback circuit breaker open, skipping callback",
			zap.String("message_id", cb.MessageID),
			zap.String("event_type", cb.EventType))

		// Schedule retry after circuit breaker timeout
		retryDelay := time.Duration(c.config.CircuitBreakerOpenTimeoutSecs) * time.Second
		nextRetry := time.Now().Add(retryDelay)
		c.queue.ScheduleCallbackRetry(cb.ID, nextRetry)

		c.logger.Info("callback rescheduled due to circuit breaker",
			zap.String("message_id", cb.MessageID),
			zap.Time("next_retry", nextRetry))
		return
	}

	c.logger.Info("sending callback",
		zap.String("message_id", cb.MessageID),
		zap.String("event_type", cb.EventType),
		zap.Int("attempt", cb.Attempts+1))

	// Parse payload
	var payload DeliveryEventCallback
	if err := json.Unmarshal([]byte(cb.Payload), &payload); err != nil {
		c.logger.Error("failed to parse callback payload",
			zap.Int64("callback_id", cb.ID),
			zap.Error(err))
		// Mark as complete to prevent infinite retries
		c.queue.MarkCallbackComplete(cb.ID)
		return
	}

	// Send HTTP request with context
	err := c.sendHTTPCallback(c.ctx, payload)
	duration := time.Since(startTime).Seconds()

	if err == nil {
		// Success
		c.logger.Info("callback sent successfully",
			zap.String("message_id", cb.MessageID),
			zap.String("event_type", cb.EventType))

		if c.metrics != nil {
			c.metrics.RecordCallbackAttempt("success", cb.EventType, duration)
		}

		// Record success with circuit breaker
		if c.circuitBreaker != nil {
			c.circuitBreaker.RecordSuccess()
		}

		c.queue.MarkCallbackComplete(cb.ID)
	} else {
		// Failure
		c.logger.Warn("callback failed",
			zap.String("message_id", cb.MessageID),
			zap.String("event_type", cb.EventType),
			zap.Int("attempt", cb.Attempts+1),
			zap.Error(err))

		if c.metrics != nil {
			c.metrics.RecordCallbackAttempt("failure", cb.EventType, duration)
		}

		// Record failure with circuit breaker (only for network/timeout errors)
		if c.circuitBreaker != nil {
			isNetworkError := isCallbackNetworkError(err)
			if isNetworkError {
				c.circuitBreaker.RecordFailure()
			}
		}

		// Check if callback has expired (age-based instead of attempt-based)
		maxAge := time.Duration(c.config.MaxCallbackAgeHours) * time.Hour
		age := time.Since(cb.CreatedAt)

		if age >= maxAge {
			// Give up after max age
			c.logger.Error("callback expired after max age",
				zap.String("message_id", cb.MessageID),
				zap.Int("attempts", cb.Attempts+1),
				zap.Duration("age", age),
				zap.Duration("max_age", maxAge))

			c.queue.MarkCallbackComplete(cb.ID)
		} else {
			// Calculate exponential backoff delay
			retryDelay := c.calculateRetryDelay(cb.Attempts + 1)
			nextRetry := time.Now().Add(retryDelay)

			// Don't schedule retry if it would exceed max age
			if time.Until(nextRetry) > maxAge-age {
				c.logger.Warn("callback retry would exceed max age, marking as complete",
					zap.String("message_id", cb.MessageID),
					zap.Duration("age", age),
					zap.Duration("max_age", maxAge))
				c.queue.MarkCallbackComplete(cb.ID)
			} else {
				c.queue.ScheduleCallbackRetry(cb.ID, nextRetry)

				c.logger.Info("callback retry scheduled with exponential backoff",
					zap.String("message_id", cb.MessageID),
					zap.Int("attempt", cb.Attempts+1),
					zap.Duration("retry_delay", retryDelay),
					zap.Time("next_retry", nextRetry))
			}
		}
	}
}

// calculateRetryDelay calculates exponential backoff delay for callback retries.
//
// The delay grows exponentially with each attempt:
//   - Attempt 1: initial_retry_delay_seconds (default 30s)
//   - Attempt 2: 30s * backoff_multiplier (default 60s)
//   - Attempt 3: 60s * backoff_multiplier (default 120s)
//   - ...
//   - Maximum: max_retry_delay_seconds (default 3600s / 1 hour)
//
// Parameters:
//   - attemptNumber: The attempt number (1-based)
//
// Returns:
//
//	The calculated delay duration, capped at max_retry_delay_seconds.
func (c *CallbackHandler) calculateRetryDelay(attemptNumber int) time.Duration {
	// Start with initial delay
	delay := float64(c.config.InitialRetryDelaySeconds)

	// Apply exponential backoff: delay * (multiplier ^ (attempt - 1))
	for i := 1; i < attemptNumber; i++ {
		delay *= c.config.BackoffMultiplier
	}

	// Cap at max delay
	maxDelay := float64(c.config.MaxRetryDelaySeconds)
	if delay > maxDelay {
		delay = maxDelay
	}

	return time.Duration(delay) * time.Second
}

// sendHTTPCallback sends the HTTP POST request to the webhook endpoint.
//
// The method:
//  1. Marshals the payload to JSON
//  2. Creates HTTP POST request with context (for cancellation)
//  3. Sets Content-Type and Authorization headers
//  4. Sends request (respects context cancellation and client timeout)
//  5. Validates HTTP status code (success: 2xx)
//
// Parameters:
//   - ctx: Context for request cancellation (inherited from handler context)
//   - payload: Delivery event callback data to send
//
// Returns:
//   - nil on success (2xx response)
//   - error describing the failure (network error, timeout, non-2xx status)
func (c *CallbackHandler) sendHTTPCallback(ctx context.Context, payload DeliveryEventCallback) error {
	// Marshal payload
	jsonData, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("failed to marshal payload: %w", err)
	}

	// Create request with context
	req, err := http.NewRequestWithContext(ctx, "POST", c.config.WebhookURL, bytes.NewBuffer(jsonData))
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")

	// Add auth token if configured
	if c.config.AuthToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.config.AuthToken)
	}

	// Send request (will be canceled if context is done)
	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	// Check status code
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("callback returned status %d", resp.StatusCode)
	}

	return nil
}

// isCallbackNetworkError determines if an error is a network/timeout error
// that should trigger the circuit breaker.
//
// The circuit breaker should open on infrastructure failures (network issues,
// DNS failures, timeouts) but NOT on application errors (HTTP 5xx from webhook).
// This distinction ensures that temporary application issues don't trigger the
// circuit breaker, while actual infrastructure problems do.
//
// Parameters:
//   - err: The error to classify
//
// Returns:
//   - true if the error indicates infrastructure failure (should trigger circuit breaker)
//   - false if the error is nil or indicates application failure
func isCallbackNetworkError(err error) bool {
	if err == nil {
		return false
	}

	errStr := strings.ToLower(err.Error())

	// Network errors that should trigger circuit breaker
	networkKeywords := []string{
		"connection refused",
		"connection reset",
		"connection timeout",
		"i/o timeout",
		"network",
		"dns",
		"no such host",
		"timeout",
		"deadline exceeded",
	}

	for _, keyword := range networkKeywords {
		if strings.Contains(errStr, keyword) {
			return true
		}
	}

	// HTTP 5xx errors from webhook endpoint should NOT trigger circuit breaker
	// (those are application errors, not infrastructure failures)
	if strings.Contains(errStr, "callback returned status 5") {
		return false
	}

	return false
}
