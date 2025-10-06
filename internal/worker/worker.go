package worker

import (
	"context"
	"fmt"
	"sync"
	"time"

	"fune/internal/callback"
	"fune/internal/config"
	"fune/internal/delivery"
	"fune/internal/queue"
	"fune/internal/recovery"

	"go.uber.org/zap"
)

// Worker processes messages from the queue
type Worker struct {
	queue           *queue.Queue
	deliverer       *delivery.Deliverer
	retryScheduler  *delivery.RetryScheduler
	mxLookup        *delivery.MXLookup
	deliveryConfig  *config.OutboundConfig
	queueConfig     *config.QueueConfig
	logger          *zap.Logger
	callbackHandler *callback.CallbackHandler
	ctx             context.Context
	cancel          context.CancelFunc
	wg              sync.WaitGroup
}

// NewWorker creates a new queue worker
func NewWorker(
	q *queue.Queue,
	deliverer *delivery.Deliverer,
	retryScheduler *delivery.RetryScheduler,
	mxLookup *delivery.MXLookup,
	callbackHandler *callback.CallbackHandler,
	deliveryCfg *config.OutboundConfig,
	queueCfg *config.QueueConfig,
	logger *zap.Logger,
) *Worker {
	ctx, cancel := context.WithCancel(context.Background())

	return &Worker{
		queue:           q,
		deliverer:       deliverer,
		retryScheduler:  retryScheduler,
		mxLookup:        mxLookup,
		callbackHandler: callbackHandler,
		deliveryConfig:  deliveryCfg,
		queueConfig:     queueCfg,
		logger:          logger,
		ctx:             ctx,
		cancel:          cancel,
	}
}

// Start begins processing the queue
func (w *Worker) Start(workerCount int) {
	w.logger.Info("starting workers",
		zap.Int("worker_count", workerCount))

	// Start queue processing workers
	for i := 0; i < workerCount; i++ {
		w.wg.Add(1)
		go w.processQueue(i)
	}

	// Start cleanup worker
	w.wg.Add(1)
	go w.cleanupExpired()

	w.logger.Info("all workers started")
}

// Stop gracefully shuts down workers
func (w *Worker) Stop() {
	w.logger.Info("stopping workers...")
	w.cancel()
	w.wg.Wait()
	w.logger.Info("all workers stopped")
}

// processQueue is the main worker loop
func (w *Worker) processQueue(workerID int) {
	defer w.wg.Done()
	defer recovery.RecoverPanic(w.logger, fmt.Sprintf("queue worker %d", workerID))

	w.logger.Info("queue worker started",
		zap.Int("worker_id", workerID))

	// Fallback ticker in case notifications are missed
	pollInterval := time.Duration(w.queueConfig.PollIntervalSeconds) * time.Second
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	notifyCh := w.queue.NotifyChan()

	for {
		select {
		case <-w.ctx.Done():
			w.logger.Info("queue worker stopping",
				zap.Int("worker_id", workerID))
			return

		case <-notifyCh:
			// New message notification received - process immediately
			w.processBatch(workerID)

		case <-ticker.C:
			// Periodic poll as fallback
			w.processBatch(workerID)
		}
	}
}

// processBatch processes a batch of messages
func (w *Worker) processBatch(workerID int) {
	batchSize := w.queueConfig.BatchSize
	messages, err := w.queue.GetNextMessages(batchSize)
	if err != nil {
		w.logger.Error("failed to get messages from queue",
			zap.Int("worker_id", workerID),
			zap.Error(err))
		return
	}

	if len(messages) == 0 {
		return // No messages ready
	}

	w.logger.Debug("processing batch",
		zap.Int("worker_id", workerID),
		zap.Int("message_count", len(messages)))

	// Batch DNS prefetch: Extract unique domains and prefetch MX records in parallel
	if w.mxLookup != nil && len(messages) > 1 {
		domains := make([]string, 0, len(messages))
		for _, msg := range messages {
			domains = append(domains, msg.ToDomain)
		}

		// Prefetch DNS for all domains in parallel (non-blocking, populates cache)
		w.mxLookup.BatchPrefetch(w.ctx, domains)
		// Note: We don't need the results here - the cache is populated
		// Individual deliveries will use cached results
	}

	// Process messages (DNS already cached from prefetch)
	for _, msg := range messages {
		w.processMessage(msg)
	}
}

// processMessage handles a single message
func (w *Worker) processMessage(msg *queue.QueuedMessage) {
	w.logger.Info("processing message",
		zap.String("message_id", msg.MessageID),
		zap.String("to", msg.ToAddr),
		zap.Int("attempt", msg.Attempts+1))

	// Mark as sending
	w.queue.UpdateStatus(msg.MessageID, queue.StatusSending)

	// Attempt delivery with worker context
	result := w.deliverer.DeliverMessage(w.ctx, msg)

	// Record the attempt
	attempt := &queue.DeliveryAttempt{
		MessageID:     msg.MessageID,
		AttemptNumber: msg.Attempts + 1,
		AttemptedAt:   time.Now(),
		MXHost:        result.MXHost,
		SourceIP:      result.SourceIP,
		SMTPCode:      result.SMTPCode,
		SMTPResponse:  result.SMTPResponse,
		Success:       result.Success,
		DurationMs:    result.DurationMs,
	}

	if result.Error != nil {
		attempt.Error = result.Error.Message
		attempt.ErrorCategory = string(result.Error.Category)
	}

	if err := w.queue.RecordAttempt(attempt); err != nil {
		w.logger.Error("failed to record attempt",
			zap.String("message_id", msg.MessageID),
			zap.Error(err))
	}

	// Handle result
	if result.Success {
		w.handleSuccess(msg, result)
	} else {
		w.handleFailure(msg, result)
	}
}

// handleSuccess processes successful delivery
func (w *Worker) handleSuccess(msg *queue.QueuedMessage, result *delivery.DeliveryResult) {
	w.logger.Info("message delivered successfully",
		zap.String("message_id", msg.MessageID),
		zap.String("to", msg.ToAddr),
		zap.String("mx_host", result.MXHost),
		zap.String("source_ip", result.SourceIP),
		zap.Int("attempts", msg.Attempts+1),
		zap.Int64("duration_ms", result.DurationMs))

	// Update final delivery info
	w.queue.UpdateDeliveryResult(msg.MessageID, result.MXHost, result.SourceIP, result.SMTPCode, result.SMTPResponse)

	// Update status
	w.queue.UpdateStatus(msg.MessageID, queue.StatusDelivered)

	// Send callback
	w.callbackHandler.EnqueueDeliveredCallback(msg, result)

	// Don't delete - let cleanup job handle it after idempotency TTL expires
	// This preserves idempotency_key for deduplication during retry window
}

// handleFailure processes failed delivery
func (w *Worker) handleFailure(msg *queue.QueuedMessage, result *delivery.DeliveryResult) {
	if result.Error == nil {
		w.logger.Error("delivery failed but no error provided",
			zap.String("message_id", msg.MessageID))
		return
	}

	w.logger.Warn("delivery attempt failed",
		zap.String("message_id", msg.MessageID),
		zap.String("to", msg.ToAddr),
		zap.String("error_category", string(result.Error.Category)),
		zap.Int("smtp_code", result.SMTPCode),
		zap.String("error", result.Error.Message),
		zap.Int("attempts", msg.Attempts+1))

	// Check if message has expired
	if delivery.IsExpired(msg) {
		w.handleExpired(msg, result)
		return
	}

	// Handle based on error category
	switch result.Error.Category {
	case delivery.ErrorPermanent:
		w.handlePermanentFailure(msg, result)

	case delivery.ErrorThrottled:
		w.handleThrottledDelivery(msg, result)

	case delivery.ErrorTemporary, delivery.ErrorGreylist, delivery.ErrorNetwork:
		w.handleTemporaryFailure(msg, result)

	default:
		w.handleTemporaryFailure(msg, result)
	}
}

// handlePermanentFailure processes 5xx permanent errors
func (w *Worker) handlePermanentFailure(msg *queue.QueuedMessage, result *delivery.DeliveryResult) {
	w.logger.Info("permanent failure - hard bounce",
		zap.String("message_id", msg.MessageID),
		zap.String("to", msg.ToAddr),
		zap.Int("smtp_code", result.SMTPCode),
		zap.String("response", result.SMTPResponse))

	// Update final delivery info
	w.queue.UpdateDeliveryResult(msg.MessageID, result.MXHost, result.SourceIP, result.SMTPCode, result.SMTPResponse)

	// Update status
	w.queue.UpdateStatus(msg.MessageID, queue.StatusHardBounce)

	// Send callback
	w.callbackHandler.EnqueueHardBounceCallback(msg, result)

	// Don't delete - let cleanup job handle it after idempotency TTL expires
	// This preserves idempotency_key for deduplication during retry window
}

// handleThrottledDelivery schedules quick retry for rate-limited deliveries
func (w *Worker) handleThrottledDelivery(msg *queue.QueuedMessage, result *delivery.DeliveryResult) {
	// Use configured throttle retry delay (default 5 seconds)
	nextRetry := time.Now().Add(time.Duration(w.deliveryConfig.PerDomainRetrySeconds) * time.Second)

	w.logger.Debug("delivery throttled, scheduling quick retry",
		zap.String("message_id", msg.MessageID),
		zap.String("domain", msg.ToDomain),
		zap.Time("next_retry", nextRetry),
		zap.Duration("delay", time.Until(nextRetry)))

	err := w.queue.ScheduleRetry(
		msg.MessageID,
		nextRetry,
		msg.Attempts, // Don't increment attempts for throttled deliveries
		result.Error.Message,
		0, // No SMTP code for throttled
		"",
	)

	if err != nil {
		w.logger.Error("failed to schedule throttled retry",
			zap.String("message_id", msg.MessageID),
			zap.Error(err))
	}
}

// handleTemporaryFailure schedules retry for temporary errors
func (w *Worker) handleTemporaryFailure(msg *queue.QueuedMessage, result *delivery.DeliveryResult) {
	newAttempts := msg.Attempts + 1

	// Check if we've exceeded max age
	if time.Now().After(msg.ExpiresAt) {
		w.logger.Info("message expired after temporary failures",
			zap.String("message_id", msg.MessageID),
			zap.Int("total_attempts", newAttempts),
			zap.Duration("age", time.Since(msg.CreatedAt)))

		// Update status
		w.queue.UpdateStatus(msg.MessageID, queue.StatusTempExpired)

		// Send callback
		w.callbackHandler.EnqueueTempExpiredCallback(msg, result)

		// Don't delete - let cleanup job handle it after idempotency TTL expires
		// This preserves idempotency_key for deduplication during retry window
		return
	}

	// Schedule retry
	nextRetry := w.retryScheduler.GetNextRetryTime(newAttempts, result.Error.Category)

	w.logger.Info("scheduling retry",
		zap.String("message_id", msg.MessageID),
		zap.Int("attempt", newAttempts),
		zap.Time("next_retry", nextRetry),
		zap.Duration("delay", time.Until(nextRetry)),
		zap.String("error_category", string(result.Error.Category)))

	err := w.queue.ScheduleRetry(
		msg.MessageID,
		nextRetry,
		newAttempts,
		result.Error.Message,
		result.SMTPCode,
		result.SMTPResponse,
	)

	if err != nil {
		w.logger.Error("failed to schedule retry",
			zap.String("message_id", msg.MessageID),
			zap.Error(err))
	}
}

// handleExpired processes messages that have exceeded their lifetime
func (w *Worker) handleExpired(msg *queue.QueuedMessage, result *delivery.DeliveryResult) {
	w.logger.Info("message expired",
		zap.String("message_id", msg.MessageID),
		zap.String("to", msg.ToAddr),
		zap.Int("attempts", msg.Attempts+1),
		zap.Duration("age", time.Since(msg.CreatedAt)))

	// Update status
	w.queue.UpdateStatus(msg.MessageID, queue.StatusExpired)

	// Send callback
	w.callbackHandler.EnqueueExpiredCallback(msg, result)

	// Don't delete - let cleanup job handle it after idempotency TTL expires
	// This preserves idempotency_key for deduplication during retry window
}

// cleanupExpired periodically cleans up expired messages
func (w *Worker) cleanupExpired() {
	defer w.wg.Done()
	defer recovery.RecoverPanic(w.logger, "cleanup worker")

	w.logger.Info("cleanup worker started")

	ticker := time.NewTicker(time.Duration(w.queueConfig.CleanupIntervalSeconds) * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-w.ctx.Done():
			w.logger.Info("cleanup worker stopping")
			return

		case <-ticker.C:
			w.performCleanup()
		}
	}
}

// performCleanup finds and processes expired messages
func (w *Worker) performCleanup() {
	expired, err := w.queue.FindExpiredMessages()
	if err != nil {
		w.logger.Error("failed to find expired messages",
			zap.Error(err))
		return
	}

	if len(expired) == 0 {
		return
	}

	w.logger.Info("found expired messages",
		zap.Int("count", len(expired)))

	for _, msg := range expired {
		w.logger.Info("marking message as expired",
			zap.String("message_id", msg.MessageID),
			zap.String("to", msg.ToAddr),
			zap.Duration("age", time.Since(msg.CreatedAt)))

		// Update status
		w.queue.UpdateStatus(msg.MessageID, queue.StatusExpired)

		// Send callback
		w.callbackHandler.EnqueueExpiredCallback(msg, nil)

		// Don't delete - let cleanup job handle it after idempotency TTL expires
		// This preserves idempotency_key for deduplication during retry window
	}
}
