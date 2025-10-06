package handler

import (
	"encoding/json"
	"net/http"
	"runtime"
	"time"

	"fune/internal/delivery"
	"fune/internal/gossip"
	"fune/internal/queue"

	"go.uber.org/zap"
)

// HealthHandler provides comprehensive health and status information
type HealthHandler struct {
	gossip    *gossip.Gossip
	queue     *queue.Queue
	deliverer *delivery.Deliverer
	startTime time.Time
	logger    *zap.Logger
}

// NewHealthHandler creates a new health handler
func NewHealthHandler(g *gossip.Gossip, q *queue.Queue, d *delivery.Deliverer, logger *zap.Logger) *HealthHandler {
	return &HealthHandler{
		gossip:    g,
		queue:     q,
		deliverer: d,
		startTime: time.Now(),
		logger:    logger,
	}
}

// HealthResponse represents the comprehensive health check response
type HealthResponse struct {
	Status    string                `json:"status"` // "healthy", "degraded", "unhealthy"
	Timestamp string                `json:"timestamp"`
	Uptime    string                `json:"uptime"`
	Queue     QueueHealth           `json:"queue"`
	Cluster   *ClusterHealth        `json:"cluster,omitempty"`
	Circuit   *CircuitBreakerHealth `json:"circuit_breaker,omitempty"`
	System    SystemHealth          `json:"system"`
}

// QueueHealth represents queue status
type QueueHealth struct {
	Pending   int64 `json:"pending"`
	Active    int64 `json:"active"`
	Failed    int64 `json:"failed"`
	Delivered int64 `json:"delivered"`
	Total     int64 `json:"total"`
}

// ClusterHealth represents cluster status
type ClusterHealth struct {
	Enabled   bool                         `json:"enabled"`
	NodeCount int                          `json:"node_count"`
	Leader    string                       `json:"leader,omitempty"`
	Nodes     map[string]ClusterNodeStatus `json:"nodes,omitempty"`
}

// CircuitBreakerHealth represents circuit breaker status
type CircuitBreakerHealth struct {
	State         string `json:"state"` // "closed", "open", "half_open"
	Failures      uint32 `json:"failures"`
	Successes     uint32 `json:"successes"`
	LastStateTime string `json:"last_state_time"`
}

// SystemHealth represents system resource information
type SystemHealth struct {
	GoVersion     string `json:"go_version"`
	Goroutines    int    `json:"goroutines"`
	MemoryMB      uint64 `json:"memory_mb"`
	MemoryAllocMB uint64 `json:"memory_alloc_mb"`
}

// ServeHTTP handles health check requests
func (h *HealthHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	response := h.buildHealthResponse()

	w.Header().Set("Content-Type", "application/json")

	// Set HTTP status based on health
	statusCode := http.StatusOK
	if response.Status == "degraded" {
		statusCode = http.StatusOK // 200 but degraded
	} else if response.Status == "unhealthy" {
		statusCode = http.StatusServiceUnavailable
	}

	w.WriteHeader(statusCode)
	json.NewEncoder(w).Encode(response)
}

func (h *HealthHandler) buildHealthResponse() HealthResponse {
	response := HealthResponse{
		Status:    "healthy",
		Timestamp: time.Now().Format(time.RFC3339),
		Uptime:    formatDuration(time.Since(h.startTime)),
		Queue:     h.getQueueHealth(),
		System:    h.getSystemHealth(),
	}

	// Add cluster health if gossip is enabled
	if h.gossip != nil {
		response.Cluster = h.getClusterHealth()
	}

	// Add circuit breaker health if available
	if h.deliverer != nil {
		cb := h.deliverer.GetCircuitBreaker()
		if cb != nil {
			response.Circuit = h.getCircuitBreakerHealth(cb)

			// If circuit is open, mark as degraded
			if cb.GetState() == delivery.CircuitOpen {
				response.Status = "degraded"
			}
		}
	}

	// Check queue depth for degraded status
	if response.Queue.Pending > 10000 {
		response.Status = "degraded"
	}

	return response
}

func (h *HealthHandler) getQueueHealth() QueueHealth {
	if h.queue == nil {
		return QueueHealth{}
	}

	// Get queue depth (queued + sending messages)
	queueDepth, err := h.queue.GetQueueDepth()
	if err != nil {
		h.logger.Error("failed to get queue depth", zap.Error(err))
		queueDepth = 0
	}

	return QueueHealth{
		Pending: queueDepth,
		// Note: For detailed stats by status, use admin endpoints which query the database directly
		// We keep this lightweight for health checks
	}
}

func (h *HealthHandler) getClusterHealth() *ClusterHealth {
	if h.gossip == nil {
		return &ClusterHealth{Enabled: false}
	}

	clusterStatus := h.gossip.GetClusterStatus()
	leader := h.gossip.GetLeader()

	nodes := make(map[string]ClusterNodeStatus)
	for nodeID, status := range clusterStatus {
		nodes[nodeID] = ClusterNodeStatus{
			NodeID:        status.NodeID,
			QueueDepth:    status.QueueDepth,
			ActiveWorkers: status.ActiveWorkers,
			Uptime:        formatDuration(status.Uptime),
			LastSeen:      status.LastSeen.Format(time.RFC3339),
			UptimeSeconds: int64(status.Uptime.Seconds()),
		}
	}

	return &ClusterHealth{
		Enabled:   true,
		NodeCount: len(nodes),
		Leader:    leader,
		Nodes:     nodes,
	}
}

func (h *HealthHandler) getCircuitBreakerHealth(cb *delivery.CircuitBreaker) *CircuitBreakerHealth {
	stats := cb.GetStats()

	stateStr := stats["state"].(string)
	failures := stats["consecutive_failures"].(uint32)
	successes := stats["consecutive_successes"].(uint32)
	lastStateChange := stats["last_state_change"].(time.Time)

	return &CircuitBreakerHealth{
		State:         stateStr,
		Failures:      failures,
		Successes:     successes,
		LastStateTime: lastStateChange.Format(time.RFC3339),
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
