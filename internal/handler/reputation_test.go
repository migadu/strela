package handler

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"strela/internal/config"
	"strela/internal/delivery"
)

func newReputationTestDeliverer(t *testing.T, trackingEnabled bool) *delivery.Deliverer {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := &config.OutboundConfig{}
	mxLookup := delivery.NewMXLookup(&config.DNSConfig{}, logger)
	d := delivery.NewDeliverer(cfg, &config.ExpandedSourceIPs{}, mxLookup, logger,
		&config.ReputationConfig{EnableIPTracking: trackingEnabled}, nil, nil)
	t.Cleanup(d.Stop)
	return d
}

func TestReputationHandler_DegradedIPs(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	d := newReputationTestDeliverer(t, true)

	d.GetReputationTracker().MarkIPDegraded("192.0.2.10", 554, "5.7.1 Client host listed on Spamhaus ZEN",
		delivery.DeliveryInfo{From: "a@example.com", To: "b@example.org", MXHost: "mx.example.org"})

	h := NewReputationHandler(d, logger)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/reputation", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	var resp ReputationResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.Enabled {
		t.Error("enabled = false, want true")
	}
	if resp.DegradedCount != 1 || len(resp.DegradedIPs) != 1 {
		t.Fatalf("degraded_count = %d, degraded_ips = %d entries, want 1/1", resp.DegradedCount, len(resp.DegradedIPs))
	}
	ip := resp.DegradedIPs[0]
	if ip.IP != "192.0.2.10" || ip.LastSMTPCode != 554 || ip.DegradedAt == "" || ip.RetryAfter == "" {
		t.Errorf("unexpected entry: %+v", ip)
	}
}

func TestReputationHandler_EmptyAndDisabled(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	d := newReputationTestDeliverer(t, false)

	h := NewReputationHandler(d, logger)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/reputation", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var resp ReputationResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Enabled {
		t.Error("enabled = true, want false")
	}
	if resp.DegradedIPs == nil {
		t.Error("degraded_ips should encode as [], not null")
	}
	if resp.DegradedCount != 0 {
		t.Errorf("degraded_count = %d, want 0", resp.DegradedCount)
	}
}

func TestReputationHandler_MethodNotAllowed(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	d := newReputationTestDeliverer(t, true)

	h := NewReputationHandler(d, logger)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/reputation", nil))

	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", rec.Code)
	}
}
