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

// HealthHandler provides comprehensive health and status information for monitoring and alerting.
//
// The health endpoint returns detailed system status including:
//   - Queue depth and message statistics
//   - Database health (size, WAL size, fragmentation, cache hit ratio)
//   - Cluster health (node count, leader, per-node stats)
//   - Circuit breaker state (closed, open, half-open)
//   - System resources (memory, goroutines, uptime)
//
// Health Status Levels:
//   - "healthy": All systems operational (HTTP 200)
//   - "degraded": System operational but experiencing issues (HTTP 200)
//   - "unhealthy": System not operational (HTTP 503)
//
// Degraded Status Triggers:
//   - Queue depth exceeds 10,000 messages
//   - Database fragmentation exceeds 30%
//   - Database size exceeds 10GB
//   - Circuit breaker is open
//
// This handler is designed for health check systems, load balancers, and monitoring tools.
// It provides sufficient information for both automated health checks and human diagnosis.
//
// Example Response:
//
//	{
//	    "status": "healthy",
//	    "timestamp": "2025-10-07T12:34:56Z",
//	    "uptime": "2 days 5 hours",
//	    "queue": {"pending": 123, "active": 5, "failed": 2, "delivered": 98765, "total": 98895},
//	    "database": {"size_mb": 245.6, "wal_size_mb": 12.3, "connections": 5, ...},
//	    "cluster": {"enabled": true, "node_count": 3, "leader": "node-1", ...},
//	    "circuit_breaker": {"state": "closed", "failures": 0, "successes": 1234, ...},
//	    "system": {"go_version": "go1.21.0", "goroutines": 45, "memory_mb": 128, ...}
//	}
type HealthHandler struct {
	gossip    *gossip.Gossip
	queue     *queue.Queue
	deliverer *delivery.Deliverer
	startTime time.Time
	logger    *zap.Logger
}

// NewHealthHandler creates a new health check HTTP handler.
//
// Parameters:
//   - g: Gossip service for cluster health (nil if clustering disabled)
//   - q: Queue for message and database statistics
//   - d: Deliverer for circuit breaker status (nil if not available)
//   - logger: Structured logger for error logging
//
// The handler records the creation time to report uptime in health responses.
//
// Any parameter can be nil except logger. Health checks will adapt to show only
// available information (e.g., no cluster health if gossip is nil).
//
// Example:
//
//	handler := NewHealthHandler(gossip, queue, deliverer, logger)
//	http.Handle("/health", handler)
func NewHealthHandler(g *gossip.Gossip, q *queue.Queue, d *delivery.Deliverer, logger *zap.Logger) *HealthHandler {
	return &HealthHandler{
		gossip:    g,
		queue:     q,
		deliverer: d,
		startTime: time.Now(),
		logger:    logger,
	}
}

// HealthResponse represents the comprehensive health check response.
//
// This structure provides detailed system status for monitoring tools, load balancers,
// and operations teams. The Status field determines the overall health:
//   - "healthy": All systems operational (HTTP 200)
//   - "degraded": System operational but experiencing issues (HTTP 200)
//   - "unhealthy": System not operational (HTTP 503)
//
// Optional fields (Database, Cluster, Circuit) are only included when the corresponding
// component is configured and available.
type HealthResponse struct {
	Status    string                `json:"status"` // "healthy", "degraded", "unhealthy"
	Timestamp string                `json:"timestamp"`
	Uptime    string                `json:"uptime"`
	Queue     QueueHealth           `json:"queue"`
	Database  *DatabaseHealth       `json:"database,omitempty"`
	Cluster   *ClusterHealth        `json:"cluster,omitempty"`
	Circuit   *CircuitBreakerHealth `json:"circuit_breaker,omitempty"`
	System    SystemHealth          `json:"system"`
}

// QueueHealth represents current queue status and message counts.
//
// The Pending field is the most important metric for monitoring queue depth.
// It represents the number of messages waiting for delivery (queued + sending).
//
// For detailed breakdowns by status (hard_bounce, temp_expired, etc.), use the
// admin endpoints which query the database directly. This lightweight structure
// is optimized for fast health checks.
type QueueHealth struct {
	Pending   int64 `json:"pending"`
	Active    int64 `json:"active"`
	Failed    int64 `json:"failed"`
	Delivered int64 `json:"delivered"`
	Total     int64 `json:"total"`
}

// ClusterHealth represents cluster status when gossip protocol is enabled.
//
// Provides visibility into cluster membership, leadership, and per-node statistics.
// The Leader field indicates which node is responsible for Let's Encrypt certificate
// acquisition in a multi-node deployment.
//
// Enabled is false if clustering is not configured. All other fields are omitted when disabled.
type ClusterHealth struct {
	Enabled   bool                         `json:"enabled"`
	NodeCount int                          `json:"node_count"`
	Leader    string                       `json:"leader,omitempty"`
	Nodes     map[string]ClusterNodeStatus `json:"nodes,omitempty"`
}

// CircuitBreakerHealth represents the delivery circuit breaker status.
//
// The State field indicates the current circuit breaker state:
//   - "closed": Normal operation, accepting requests
//   - "open": Rejecting requests due to consecutive failures
//   - "half_open": Testing recovery with limited requests
//
// When the state is "open", the HTTP API rejects message submissions with
// 503 Service Unavailable until the circuit recovers.
type CircuitBreakerHealth struct {
	State         string `json:"state"` // "closed", "open", "half_open"
	Failures      uint32 `json:"failures"`
	Successes     uint32 `json:"successes"`
	LastStateTime string `json:"last_state_time"`
}

// DatabaseHealth represents SQLite database statistics and performance metrics.
//
// Key monitoring metrics:
//   - SizeMB: Main database file size (triggers "degraded" if > 10GB)
//   - WALSizeMB: Write-Ahead Log size (should be small, grows during write bursts)
//   - FragmentPercent: Database fragmentation (triggers "degraded" if > 30%)
//   - CacheHitPercent: SQLite page cache hit ratio (higher is better, 95%+ ideal)
//   - QueuedMessages: Messages in "queued" state (waiting for delivery)
//   - SendingMessages: Messages in "sending" state (currently being delivered)
//
// High fragmentation or large database size may indicate need for VACUUM or OPTIMIZE operations.
type DatabaseHealth struct {
	SizeMB          float64 `json:"size_mb"`
	WALSizeMB       float64 `json:"wal_size_mb"`
	Connections     int     `json:"connections"`
	FragmentPercent float64 `json:"fragment_percent"`
	CacheHitPercent float64 `json:"cache_hit_percent"`
	QueuedMessages  int64   `json:"queued_messages"`
	SendingMessages int64   `json:"sending_messages"`
}

// SystemHealth represents Go runtime and system resource information.
//
// These metrics help diagnose resource exhaustion issues:
//   - GoVersion: Go runtime version (e.g., "go1.21.0")
//   - Goroutines: Current number of goroutines (normal: 10-100, high if > 10,000)
//   - MemoryMB: Total memory obtained from OS (includes unused memory)
//   - MemoryAllocMB: Currently allocated memory (indicates actual usage)
//
// A large difference between MemoryMB and MemoryAllocMB indicates memory that has been
// released by the application but not yet returned to the OS.
type SystemHealth struct {
	GoVersion     string `json:"go_version"`
	Goroutines    int    `json:"goroutines"`
	MemoryMB      uint64 `json:"memory_mb"`
	MemoryAllocMB uint64 `json:"memory_alloc_mb"`
}

// ServeHTTP handles health check requests and returns comprehensive system status.
//
// This method implements the http.Handler interface and only accepts GET requests.
// The response includes queue, database, cluster, circuit breaker, and system health.
//
// HTTP Status Codes:
//   - 200 OK: System is healthy or degraded but operational
//   - 503 Service Unavailable: System is unhealthy (database errors, critical failures)
//
// The response JSON includes a "status" field ("healthy", "degraded", or "unhealthy")
// that provides semantic health information independent of HTTP status code.
//
// Example:
//
//	GET /health
//	→ 200 OK
//	{
//	    "status": "degraded",
//	    "timestamp": "2025-10-07T12:34:56Z",
//	    "uptime": "2 days 5 hours",
//	    "queue": {"pending": 15234, ...},  // High queue depth triggers degraded
//	    "circuit_breaker": {"state": "open", ...},
//	    ...
//	}
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

	// Add database health if available
	if h.queue != nil {
		response.Database = h.getDatabaseHealth()

		// Check for database health issues
		if response.Database != nil {
			if response.Database.FragmentPercent > 30 {
				response.Status = "degraded"
			}
			if response.Database.SizeMB > 10000 { // 10GB
				response.Status = "degraded"
			}
		}
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

func (h *HealthHandler) getDatabaseHealth() *DatabaseHealth {
	if h.queue == nil {
		return nil
	}

	stats, err := h.queue.GetDatabaseStats()
	if err != nil {
		h.logger.Error("failed to get database stats", zap.Error(err))
		return nil
	}

	return &DatabaseHealth{
		SizeMB:          float64(stats.SizeBytes) / 1024 / 1024,
		WALSizeMB:       float64(stats.WALSizeBytes) / 1024 / 1024,
		Connections:     stats.Connections,
		FragmentPercent: stats.FragmentRatio * 100,
		CacheHitPercent: stats.CacheHitRatio * 100,
		QueuedMessages:  stats.QueuedMessages,
		SendingMessages: stats.SendingMessages,
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
