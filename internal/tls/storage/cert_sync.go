package storage

import (
	"context"
	"log/slog"
	"time"

	"fune/internal/recovery"
)

// CertSyncWorker manages periodic synchronization of certificates from local cache to S3.
type CertSyncWorker struct {
	fallbackCache *FallbackCache
	syncInterval  time.Duration
	logger        *slog.Logger
	stopCh        chan struct{}
	doneCh        chan struct{}
}

// NewCertSyncWorker creates a new certificate sync worker.
func NewCertSyncWorker(fallbackCache *FallbackCache, syncInterval time.Duration, logger *slog.Logger) *CertSyncWorker {
	return &CertSyncWorker{
		fallbackCache: fallbackCache,
		syncInterval:  syncInterval,
		logger:        logger,
		stopCh:        make(chan struct{}),
		doneCh:        make(chan struct{}),
	}
}

// Start begins the periodic sync worker in a background goroutine.
func (w *CertSyncWorker) Start() {
	w.logger.Info("starting certificate sync worker", "interval", w.syncInterval)

	recovery.SafeGo(w.logger, "cert-sync-worker", func() {
		defer close(w.doneCh)

		ticker := time.NewTicker(w.syncInterval)
		defer ticker.Stop()

		// Run initial sync immediately
		w.runSync()

		for {
			select {
			case <-ticker.C:
				w.runSync()
			case <-w.stopCh:
				w.logger.Info("certificate sync worker stopped")
				return
			}
		}
	})
}

// Stop gracefully shuts down the sync worker.
func (w *CertSyncWorker) Stop(timeout time.Duration) {
	w.logger.Info("stopping certificate sync worker")
	close(w.stopCh)

	// Wait for worker to finish with timeout
	select {
	case <-w.doneCh:
		w.logger.Info("certificate sync worker stopped gracefully")
	case <-time.After(timeout):
		w.logger.Warn("certificate sync worker stop timed out")
	}
}

// runSync performs a single sync operation.
func (w *CertSyncWorker) runSync() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	w.logger.Debug("running certificate sync")
	if err := w.fallbackCache.SyncAllToS3(ctx); err != nil {
		w.logger.Error("certificate sync failed", "error", err)
	}
}
