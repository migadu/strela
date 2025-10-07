// Package worker implements the asynchronous message delivery worker pool.
//
// The worker package provides a multi-worker architecture for processing queued messages
// and attempting SMTP delivery. Workers poll the message queue (with instant channel-based
// notifications), retrieve batches of messages ready for delivery, and coordinate with the
// delivery engine to send messages to their destination MX servers.
//
// # Architecture
//
// The worker pool consists of:
//   - N delivery workers that process messages from the queue
//   - 1 cleanup worker that handles expired messages
//   - Channel-based notification system for instant message processing
//   - Fallback polling mechanism (default 30s) to ensure no messages are missed
//
// # Worker Lifecycle
//
// Each worker follows this pattern:
//  1. Wait for notification (via channel) or poll interval
//  2. Dequeue a batch of messages ready for delivery
//  3. Prefetch DNS MX records for all domains in parallel
//  4. Deliver each message using the delivery engine
//  5. Record delivery attempt with result details
//  6. Update message status based on delivery outcome:
//     - Success: Mark as delivered, enqueue success callback
//     - Permanent failure (5xx): Mark as hard bounce, enqueue bounce callback
//     - Temporary failure (4xx): Schedule retry with exponential backoff
//     - Throttled: Schedule quick retry (default 5s)
//     - Expired: Mark as expired, enqueue expired callback
//
// # Error Handling
//
// Workers handle different delivery outcomes:
//   - Permanent errors: Immediate hard bounce, no retry
//   - Temporary errors: Exponential backoff retry (5min → 10min → 20min → ... → 12hr)
//   - Greylisting (421): Fast retry (2 minutes)
//   - Throttling: Per-domain rate limit retry (configurable, default 5s)
//   - Expired messages: Messages exceeding max age (default 48h)
//
// # Graceful Shutdown
//
// Workers support graceful shutdown via context cancellation. In-flight message
// processing completes before the worker exits, ensuring no message loss.
//
// # Concurrency
//
// Multiple workers can run concurrently. The queue's Dequeue operation is
// atomic, ensuring each message is processed by exactly one worker.
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

// Worker processes messages from the queue and attempts SMTP delivery.
//
// Worker manages a pool of goroutines that continuously poll the message queue,
// retrieve messages ready for delivery, and coordinate with the delivery engine
// to send emails to their destination MX servers. It handles delivery results,
// schedules retries for temporary failures, and enqueues webhook callbacks for
// delivery events.
//
// Workers use a hybrid polling approach: instant channel-based notifications when
// messages are enqueued (for low-latency processing) combined with periodic polling
// as a fallback mechanism to ensure reliability.
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

// NewWorker creates a new queue worker instance.
//
// The worker is initialized with all necessary dependencies for message processing:
//   - queue: Message queue for retrieving and updating messages
//   - deliverer: Delivery engine for sending messages via SMTP
//   - retryScheduler: Calculates exponential backoff retry times
//   - mxLookup: DNS MX record resolver with caching
//   - callbackHandler: Webhook callback dispatcher for delivery events
//   - deliveryCfg: Outbound SMTP configuration (timeouts, source IPs, etc.)
//   - queueCfg: Queue configuration (batch size, poll interval, etc.)
//   - logger: Structured logger for observability
//
// The returned worker is ready to start but not yet running. Call Start() to
// begin processing messages.
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

// Start begins processing the message queue with the specified number of workers.
//
// This method spawns workerCount delivery workers that process messages from the
// queue concurrently, plus one additional cleanup worker that handles expired messages.
// Each worker runs in its own goroutine and continues processing until Stop() is called.
//
// Parameters:
//   - workerCount: Number of concurrent delivery workers (typical: 5-20)
//
// The method returns immediately after spawning all worker goroutines. Workers will
// continue running in the background until Stop() is called.
//
// Example:
//
//	worker := NewWorker(queue, deliverer, ...)
//	worker.Start(10) // Start 10 delivery workers + 1 cleanup worker
//	// ... application runs ...
//	worker.Stop() // Graceful shutdown
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

// Stop gracefully shuts down all workers.
//
// This method signals all workers to stop processing via context cancellation and
// waits for them to finish their current work. In-flight message deliveries are
// allowed to complete before the workers exit.
//
// Stop is blocking and will not return until all workers have fully stopped.
// It is safe to call Stop multiple times.
func (w *Worker) Stop() {
	w.logger.Info("stopping workers...")
	w.cancel()
	w.wg.Wait()
	w.logger.Info("all workers stopped")
}

// processQueue is the main worker loop that continuously processes messages.
//
// Each worker runs this method in a separate goroutine. The loop waits for either:
//   - A notification on the queue's notification channel (instant processing)
//   - A tick from the fallback poll timer (default 30s)
//   - Context cancellation (graceful shutdown)
//
// When triggered, the worker calls processBatch to retrieve and deliver messages.
// This hybrid approach ensures low-latency processing (via notifications) while
// maintaining reliability (via periodic polling).
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

// processBatch retrieves and processes a batch of messages ready for delivery.
//
// This method:
//  1. Retrieves up to BatchSize messages from the queue
//  2. Extracts unique domains and prefetches their MX records in parallel
//  3. Processes each message sequentially (delivery is already parallelized via worker pool)
//
// The DNS prefetch optimization significantly improves batch processing performance
// by populating the MX cache before individual deliveries begin.
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

// processMessage handles the complete lifecycle of delivering a single message.
//
// The method:
//  1. Marks the message as "sending" in the queue
//  2. Attempts delivery via the delivery engine
//  3. Records the delivery attempt with all result details
//  4. Updates message status and schedules callbacks based on the outcome
//
// All delivery attempts are recorded in the database for auditing, regardless
// of success or failure.
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

// handleSuccess processes a successful message delivery.
//
// On success:
//   - Updates message status to "delivered"
//   - Records final delivery details (MX host, source IP, SMTP response)
//   - Enqueues a "delivered" webhook callback
//   - Preserves the message for idempotency until cleanup job removes it
//
// Messages are not immediately deleted to maintain idempotency protection during
// the configured idempotency TTL window.
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

// handleFailure processes a failed delivery attempt.
//
// The method inspects the error category and routes to the appropriate handler:
//   - ErrorPermanent: Hard bounce (5xx SMTP codes) - no retry
//   - ErrorThrottled: Per-domain rate limit hit - quick retry (default 5s)
//   - ErrorTemporary: Temporary failure (4xx) - exponential backoff retry
//   - ErrorGreylist: Greylisting detected (421) - fast retry (2 minutes)
//   - ErrorNetwork: Network/DNS issues - exponential backoff retry
//
// If the message has exceeded its maximum age, it is marked as expired instead
// of being retried.
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

// handlePermanentFailure processes permanent SMTP delivery failures.
//
// Permanent failures (5xx SMTP response codes) indicate the message cannot be
// delivered and should not be retried. Common examples:
//   - 550: Mailbox unavailable or rejected
//   - 551: User not local
//   - 552: Mailbox full
//   - 554: Transaction failed
//
// The message is marked as "hard_bounce" and a hard bounce callback is enqueued.
// No retry is scheduled.
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

// handleThrottledDelivery schedules a quick retry for rate-limited deliveries.
//
// When a per-domain rate limit is hit (internal throttling mechanism), the message
// is rescheduled for retry after a short delay (default 5 seconds, configured via
// per_domain_retry_seconds). The attempt counter is NOT incremented for throttled
// deliveries, as they haven't actually attempted SMTP delivery.
//
// This differs from greylist handling, which uses a longer retry delay and is
// triggered by remote SMTP server responses.
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

// handleTemporaryFailure schedules a retry with exponential backoff.
//
// Temporary failures include:
//   - 4xx SMTP response codes (temporary errors)
//   - Network errors (connection failures, timeouts)
//   - DNS resolution failures
//   - Greylisting (421 response)
//
// The retry scheduler calculates the next retry time using exponential backoff:
//   - Attempt 1: 5 minutes
//   - Attempt 2: 10 minutes
//   - Attempt 3: 20 minutes
//   - ...
//   - Maximum: 12 hours
//
// Greylisting errors use a shorter retry delay (default 2 minutes).
//
// If the message would expire before the next retry, it is marked as "temp_expired"
// instead of being rescheduled.
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

// handleExpired processes messages that have exceeded their maximum age.
//
// Messages are considered expired when:
//   - They have been in the queue longer than max_message_age_hours (default 48h)
//   - The next retry would occur after the expiration time
//
// Expired messages are marked with status "expired" and an expired callback is
// enqueued. No further delivery attempts are made.
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

// cleanupExpired periodically scans for and processes expired messages.
//
// This goroutine runs independently from delivery workers and wakes up at regular
// intervals (default 60 seconds, configured via cleanup_interval_seconds) to find
// messages that have exceeded their maximum age but haven't been marked as expired
// yet (e.g., messages still in "queued" state past their expiration time).
//
// The cleanup worker ensures that all expired messages eventually receive proper
// status updates and callbacks, even if they were never attempted due to system
// issues or queue backlogs.
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

// performCleanup executes a single cleanup cycle.
//
// This method queries the database for messages that have exceeded their expiration
// time but are still in "queued" or "sending" status. Each expired message is marked
// as "expired" and an expired callback is enqueued.
//
// The cleanup process does not delete messages immediately; they are preserved for
// idempotency until the message retention period expires.
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
