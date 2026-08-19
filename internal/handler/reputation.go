package handler

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"sort"
	"time"

	"strela/internal/delivery"
)

// ReputationHandler exposes the in-memory IP reputation state (degraded source
// IPs with the SMTP evidence that degraded them) for admin tooling such as
// fernrohr. State is per-instance and lost on restart.
type ReputationHandler struct {
	deliverer *delivery.Deliverer
	logger    *slog.Logger
}

// NewReputationHandler creates a new reputation status HTTP handler.
func NewReputationHandler(d *delivery.Deliverer, logger *slog.Logger) *ReputationHandler {
	return &ReputationHandler{deliverer: d, logger: logger}
}

// ReputationResponse is the JSON body of GET /reputation.
type ReputationResponse struct {
	Enabled       bool         `json:"enabled"`
	DegradedCount int          `json:"degraded_count"`
	DegradedIPs   []DegradedIP `json:"degraded_ips"`
}

// DegradedIP describes one degraded source IP and the evidence behind it.
type DegradedIP struct {
	IP               string `json:"ip"`
	DegradedAt       string `json:"degraded_at"`
	RetryAfter       string `json:"retry_after"`
	FailureCount     int    `json:"failure_count"`
	LastError        string `json:"last_error,omitempty"`
	LastSMTPCode     int    `json:"last_smtp_code,omitempty"`
	LastSMTPResponse string `json:"last_smtp_response,omitempty"`
}

// ServeHTTP handles reputation status requests.
func (h *ReputationHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	tracker := h.deliverer.GetReputationTracker()
	response := ReputationResponse{
		Enabled:     tracker.Enabled(),
		DegradedIPs: []DegradedIP{},
	}
	for ip, info := range tracker.GetDegradedIPs() {
		response.DegradedIPs = append(response.DegradedIPs, DegradedIP{
			IP:               ip,
			DegradedAt:       info.DegradedAt.Format(time.RFC3339),
			RetryAfter:       info.RetryAfter.Format(time.RFC3339),
			FailureCount:     info.FailureCount,
			LastError:        info.LastFailureError,
			LastSMTPCode:     info.LastSMTPCode,
			LastSMTPResponse: info.LastSMTPResponse,
		})
	}
	sort.Slice(response.DegradedIPs, func(i, j int) bool {
		return response.DegradedIPs[i].IP < response.DegradedIPs[j].IP
	})
	response.DegradedCount = len(response.DegradedIPs)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(response); err != nil {
		h.logger.Error("failed to encode reputation response", "error", err)
	}
}
