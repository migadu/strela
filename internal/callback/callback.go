package callback

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"fune/internal/config"
	"fune/internal/delivery"
	"fune/internal/queue"
	"fune/internal/recovery"

	"go.uber.org/zap"
)

// CallbackMetrics interface for recording callback metrics
type CallbackMetrics interface {
	RecordCallbackAttempt(outcome, eventType string, duration float64)
}

// CallbackHandler manages webhook callbacks to CloudFlare Worker
type CallbackHandler struct {
	queue   *queue.Queue
	config  *config.CallbacksConfig
	logger  *zap.Logger
	client  *http.Client
	ctx     context.Context
	cancel  context.CancelFunc
	wg      sync.WaitGroup
	metrics CallbackMetrics
}

// NewCallbackHandler creates a new callback handler
func NewCallbackHandler(q *queue.Queue, cfg *config.CallbacksConfig, logger *zap.Logger) *CallbackHandler {
	ctx, cancel := context.WithCancel(context.Background())

	return &CallbackHandler{
		queue:  q,
		config: cfg,
		logger: logger,
		client: &http.Client{
			Timeout: time.Duration(cfg.TimeoutSeconds) * time.Second,
		},
		ctx:    ctx,
		cancel: cancel,
	}
}

// Start begins processing callbacks
func (c *CallbackHandler) Start() {
	c.logger.Info("starting callback processor")

	c.wg.Add(1)
	go c.processCallbacks()
}

// Stop gracefully shuts down callback processor
func (c *CallbackHandler) Stop() {
	c.logger.Info("stopping callback processor...")
	c.cancel()
	c.wg.Wait()
	c.logger.Info("callback processor stopped")
}

// SetMetrics sets the metrics recorder for callbacks
func (c *CallbackHandler) SetMetrics(metrics CallbackMetrics) {
	c.metrics = metrics
}

// EnqueueDeliveredCallback queues a successful delivery callback
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

// EnqueueHardBounceCallback queues a hard bounce callback
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

// EnqueueTempExpiredCallback queues a temp failure exhausted callback
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

// EnqueueExpiredCallback queues an expired message callback
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

// processCallbacks is the main callback processing loop
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

// processBatch processes a batch of pending callbacks
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

// sendCallback attempts to send a single callback
func (c *CallbackHandler) sendCallback(cb queue.PendingCallback) {
	startTime := time.Now()

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

// calculateRetryDelay calculates exponential backoff delay for callback retries
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

// sendHTTPCallback sends the actual HTTP POST request
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
