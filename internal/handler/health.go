package handler

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"runtime"
	"time"

	"fune/internal/delivery"
)

// HealthHandler provides health and status information.
type HealthHandler struct {
	deliverer *delivery.Deliverer
	startTime time.Time
	logger    *slog.Logger
}

// NewHealthHandler creates a new health check HTTP handler.
func NewHealthHandler(d *delivery.Deliverer, logger *slog.Logger) *HealthHandler {
	return &HealthHandler{
		deliverer: d,
		startTime: time.Now(),
		logger:    logger,
	}
}

// HealthResponse represents the health check response.
type HealthResponse struct {
	Status    string         `json:"status"`
	Timestamp string         `json:"timestamp"`
	Uptime    string         `json:"uptime"`
	System    SystemHealth   `json:"system"`
	Cluster   *ClusterHealth `json:"cluster,omitempty"`
}

type SystemHealth struct {
	GoVersion     string `json:"go_version"`
	Goroutines    int    `json:"goroutines"`
	MemoryMB      uint64 `json:"memory_mb"`
	MemoryAllocMB uint64 `json:"memory_alloc_mb"`
}

type ClusterHealth struct {
	Enabled   bool   `json:"enabled"`
	NodeCount int    `json:"node_count"`
	Leader    string `json:"leader,omitempty"`
}

// ServeHTTP handles health check requests.
func (h *HealthHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	response := h.buildHealthResponse()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(response); err != nil {
		h.logger.Error("failed to encode health response", "error", err)
	}
}

func (h *HealthHandler) buildHealthResponse() HealthResponse {
	return HealthResponse{
		Status:    "healthy",
		Timestamp: time.Now().Format(time.RFC3339),
		Uptime:    time.Since(h.startTime).String(),
		System:    h.getSystemHealth(),
		Cluster:   nil, // Cluster/gossip support removed
	}
}

func (h *HealthHandler) getSystemHealth() SystemHealth {
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)

	return SystemHealth{
		GoVersion:     runtime.Version(),
		Goroutines:    runtime.NumGoroutine(),
		MemoryMB:      mem.Sys / 1024 / 1024,
		MemoryAllocMB: mem.Alloc / 1024 / 1024,
	}
}

func (h *HealthHandler) getClusterHealth() *ClusterHealth {
	// Cluster/gossip support removed in synchronous refactoring
	return nil
}
