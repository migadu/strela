package delivery

import (
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"strela/internal/config"
)

// countingReputationMetrics counts degrade/recover events so tests can assert
// how many times a degradation actually fired (e.g. TOCTOU checks).
type countingReputationMetrics struct {
	degraded  atomic.Int64
	recovered atomic.Int64
}

func (m *countingReputationMetrics) SetIPReputationDegraded(sourceIP string, degraded bool) {}

func (m *countingReputationMetrics) RecordIPReputationEvent(eventType, sourceIP string) {
	switch eventType {
	case "degraded":
		m.degraded.Add(1)
	case "recovered":
		m.recovered.Add(1)
	}
}

func newTestTracker(t *testing.T, threshold, windowMinutes int) *IPReputationTracker {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := &config.ReputationConfig{
		EnableIPTracking:        true,
		DegradedRetryHours:      48,
		DegradedIPCleanupHours:  168,
		DegradeFailureThreshold: threshold,
		DegradeWindowMinutes:    windowMinutes,
	}
	return NewIPReputationTracker(cfg, logger)
}

// weakRepErr is a threshold-gated reputation error (ImmediateDegrade == false).
func weakRepErr() *DeliveryError {
	return &DeliveryError{
		Category:         ErrorReputation,
		SMTPCode:         550,
		SMTPResponse:     "5.7.1 blocked; your IP reputation is poor",
		ImmediateDegrade: false,
	}
}

func TestReputation_WeakStrikesRequireThreshold(t *testing.T) {
	tracker := newTestTracker(t, 3, 30)
	const ip = "192.0.2.10"
	info := DeliveryInfo{From: "a@example.com", To: "b@example.org"}

	// threshold-1 strikes must not degrade.
	for i := 0; i < 2; i++ {
		tracker.RecordDeliveryAttempt(ip, false, weakRepErr(), info)
		if _, degraded := tracker.GetDegradedIPs()[ip]; degraded {
			t.Fatalf("IP degraded after %d weak strikes (threshold 3)", i+1)
		}
	}

	// The threshold-th strike degrades.
	tracker.RecordDeliveryAttempt(ip, false, weakRepErr(), info)
	if _, degraded := tracker.GetDegradedIPs()[ip]; !degraded {
		t.Fatal("IP not degraded after reaching the strike threshold")
	}
}

func TestReputation_StaleStrikesDoNotCount(t *testing.T) {
	tracker := newTestTracker(t, 3, 30)
	const ip = "192.0.2.11"
	info := DeliveryInfo{}

	// Seed two strikes older than the 30-minute window.
	old := time.Now().Add(-time.Hour)
	tracker.mu.Lock()
	tracker.repStrikes[ip] = []time.Time{old, old}
	tracker.mu.Unlock()

	// A single fresh strike: the two stale ones are pruned, so count is 1 < 3.
	tracker.RecordDeliveryAttempt(ip, false, weakRepErr(), info)
	if _, degraded := tracker.GetDegradedIPs()[ip]; degraded {
		t.Fatal("IP degraded off stale strikes that should have been pruned")
	}

	tracker.mu.RLock()
	got := len(tracker.repStrikes[ip])
	tracker.mu.RUnlock()
	if got != 1 {
		t.Fatalf("expected 1 live strike after pruning, got %d", got)
	}
}

func TestReputation_StrongKeywordDegradesImmediately(t *testing.T) {
	tracker := newTestTracker(t, 3, 30)
	const ip = "192.0.2.12"
	info := DeliveryInfo{}

	strong := &DeliveryError{
		Category:         ErrorReputation,
		SMTPCode:         554,
		SMTPResponse:     "5.7.1 listed on Spamhaus ZEN",
		ImmediateDegrade: true,
	}
	tracker.RecordDeliveryAttempt(ip, false, strong, info)

	if _, degraded := tracker.GetDegradedIPs()[ip]; !degraded {
		t.Fatal("strong-keyword reputation error did not degrade IP on first hit")
	}
}

func TestReputation_SuccessClearsStrikes(t *testing.T) {
	tracker := newTestTracker(t, 3, 30)
	const ip = "192.0.2.13"
	info := DeliveryInfo{}

	// Accumulate below-threshold strikes.
	tracker.RecordDeliveryAttempt(ip, false, weakRepErr(), info)
	tracker.RecordDeliveryAttempt(ip, false, weakRepErr(), info)

	// A success clears them.
	tracker.RecordDeliveryAttempt(ip, true, nil, info)

	tracker.mu.RLock()
	_, has := tracker.repStrikes[ip]
	tracker.mu.RUnlock()
	if has {
		t.Fatal("strikes not cleared after successful delivery")
	}

	// Two fresh strikes again should still be below threshold (proving the
	// earlier ones were really gone, not just masked).
	tracker.RecordDeliveryAttempt(ip, false, weakRepErr(), info)
	tracker.RecordDeliveryAttempt(ip, false, weakRepErr(), info)
	if _, degraded := tracker.GetDegradedIPs()[ip]; degraded {
		t.Fatal("IP degraded on 2 strikes; success did not reset the counter")
	}
}

func TestReputation_ConcurrentThresholdCrossesOnce(t *testing.T) {
	const n = 10
	// threshold == n, pre-seed n-1 strikes: the first goroutine to acquire the
	// lock crosses (n-1+1 == n) and clears; the remaining n-1 rebuild to at most
	// n-1 < threshold, so no matter the interleaving exactly one degrade fires.
	tracker := newTestTracker(t, n, 30)
	metrics := &countingReputationMetrics{}
	tracker.SetMetrics(metrics)

	const ip = "192.0.2.14"
	now := time.Now()
	seed := make([]time.Time, 0, n-1)
	for i := 0; i < n-1; i++ {
		seed = append(seed, now)
	}
	tracker.mu.Lock()
	tracker.repStrikes[ip] = seed
	tracker.mu.Unlock()

	info := DeliveryInfo{}
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tracker.RecordDeliveryAttempt(ip, false, weakRepErr(), info)
		}()
	}
	wg.Wait()

	if got := metrics.degraded.Load(); got != 1 {
		t.Fatalf("expected exactly 1 degrade event under concurrency, got %d", got)
	}
	if _, degraded := tracker.GetDegradedIPs()[ip]; !degraded {
		t.Fatal("IP not degraded after concurrent threshold crossing")
	}
}

func TestReputation_DegradedIPDoesNotReAccumulateOnWeakSignals(t *testing.T) {
	tracker := newTestTracker(t, 3, 30)
	metrics := &countingReputationMetrics{}
	tracker.SetMetrics(metrics)

	const ip = "192.0.2.17"
	info := DeliveryInfo{}

	// Strong signal degrades immediately (1 degrade event).
	tracker.RecordDeliveryAttempt(ip, false, &DeliveryError{
		Category: ErrorReputation, SMTPCode: 554, ImmediateDegrade: true,
	}, info)
	if got := metrics.degraded.Load(); got != 1 {
		t.Fatalf("expected 1 degrade event after strong signal, got %d", got)
	}

	// While actively degraded (within retry window), weak signals must not
	// accumulate strikes or re-fire the degrade webhook/metric.
	for i := 0; i < 5; i++ {
		tracker.RecordDeliveryAttempt(ip, false, weakRepErr(), info)
	}
	if got := metrics.degraded.Load(); got != 1 {
		t.Fatalf("weak signals re-fired degrade for an already-degraded IP: %d events", got)
	}
	tracker.mu.RLock()
	_, hasStrikes := tracker.repStrikes[ip]
	tracker.mu.RUnlock()
	if hasStrikes {
		t.Error("strikes accumulated for an already-degraded IP")
	}
}

func TestReputation_CleanupDropsStaleStrikes(t *testing.T) {
	tracker := newTestTracker(t, 3, 30)

	staleIP := "192.0.2.15"
	freshIP := "192.0.2.16"
	tracker.mu.Lock()
	tracker.repStrikes[staleIP] = []time.Time{time.Now().Add(-time.Hour)}
	tracker.repStrikes[freshIP] = []time.Time{time.Now()}
	tracker.mu.Unlock()

	tracker.Cleanup()

	tracker.mu.RLock()
	_, staleExists := tracker.repStrikes[staleIP]
	_, freshExists := tracker.repStrikes[freshIP]
	tracker.mu.RUnlock()

	if staleExists {
		t.Error("Cleanup did not drop stale strike entry for a never-degraded IP")
	}
	if !freshExists {
		t.Error("Cleanup dropped a fresh strike entry still within the window")
	}
}
