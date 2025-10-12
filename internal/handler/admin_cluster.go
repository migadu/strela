package handler

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"fune/internal/gossip"
)

// ClusterStatusHandler provides cluster-wide status information via HTTP API.
//
// This handler exposes detailed cluster status when gossip protocol is enabled,
// including:
//   - List of all cluster nodes with their IDs
//   - Per-node queue depth and active worker counts
//   - Node uptime and last-seen timestamps
//   - Total node count
//
// The handler returns 503 Service Unavailable if gossip protocol is not enabled.
//
// Example Response:
//
//	{
//	    "timestamp": "2025-10-07T12:34:56Z",
//	    "node_count": 3,
//	    "nodes": {
//	        "node-1": {
//	            "node_id": "node-1",
//	            "queue_depth": 123,
//	            "active_workers": 5,
//	            "uptime": "2 days 5 hours",
//	            "uptime_seconds": 190800,
//	            "last_seen": "2025-10-07T12:34:55Z"
//	        },
//	        "node-2": {...},
//	        "node-3": {...}
//	    }
//	}
//
// This endpoint is useful for:
//   - Cluster health monitoring
//   - Load balancing decisions
//   - Capacity planning
//   - Debugging distributed issues
type ClusterStatusHandler struct {
	statusProvider *gossip.Gossip
	logger         *slog.Logger
}

// NewClusterStatusHandler creates a new cluster status HTTP handler.
//
// Parameters:
//   - g: Gossip service for cluster status (can be nil if clustering disabled)
//   - logger: Structured logger for error logging
//
// If the gossip service is nil, the handler will respond with 503 Service Unavailable
// to all requests.
//
// Example:
//
//	handler := NewClusterStatusHandler(gossip, logger)
//	http.Handle("/admin/cluster/status", handler)
func NewClusterStatusHandler(g *gossip.Gossip, logger *slog.Logger) *ClusterStatusHandler {
	return &ClusterStatusHandler{
		statusProvider: g,
		logger:         logger,
	}
}

// ClusterNodeStatus represents the status of a single node in the cluster.
//
// This structure provides detailed per-node metrics for monitoring and operations.
// The UptimeSeconds field is included for easy programmatic comparisons and alerting,
// while Uptime provides a human-readable duration string.
type ClusterNodeStatus struct {
	NodeID        string `json:"node_id"`
	QueueDepth    int64  `json:"queue_depth"`
	ActiveWorkers int    `json:"active_workers"`
	Uptime        string `json:"uptime"`
	LastSeen      string `json:"last_seen"`
	UptimeSeconds int64  `json:"uptime_seconds"`
}

// ClusterStatusResponse represents the complete cluster status API response.
//
// This structure aggregates status information from all nodes in the cluster,
// providing a comprehensive view of cluster health and capacity.
type ClusterStatusResponse struct {
	Timestamp string                       `json:"timestamp"`
	Nodes     map[string]ClusterNodeStatus `json:"nodes"`
	NodeCount int                          `json:"node_count"`
}

// ServeHTTP handles cluster status requests and returns node information.
//
// This method implements the http.Handler interface and only accepts GET requests.
// It queries the gossip protocol for current cluster state and returns detailed
// per-node status information.
//
// HTTP Status Codes:
//   - 200 OK: Cluster status retrieved successfully
//   - 405 Method Not Allowed: Non-GET request
//   - 503 Service Unavailable: Gossip protocol not enabled
//
// Example:
//
//	GET /admin/cluster/status
//	→ 200 OK
//	{
//	    "timestamp": "2025-10-07T12:34:56Z",
//	    "node_count": 3,
//	    "nodes": {...}
//	}
func (h *ClusterStatusHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if h.statusProvider == nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(map[string]string{
			"error": "Gossip protocol is not enabled",
		})
		return
	}

	// Get cluster status from gossip
	clusterStatus := h.statusProvider.GetClusterStatus()

	// Convert to API response format
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

	response := ClusterStatusResponse{
		Timestamp: time.Now().Format(time.RFC3339),
		Nodes:     nodes,
		NodeCount: len(nodes),
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(response)
}

// formatDuration formats a time.Duration into a human-readable string.
//
// The function automatically selects the most appropriate unit:
//   - Less than 1 minute: "45s"
//   - Less than 1 hour: "45 minutes"
//   - Less than 24 hours: "12 hours"
//   - 24 hours or more: "2 days 5 hours" or "7 days"
//
// Examples:
//   - 45 * time.Second → "45s"
//   - 90 * time.Minute → "90 minutes"
//   - 5 * time.Hour → "5 hours"
//   - 50 * time.Hour → "2 days 2 hours"
//   - 72 * time.Hour → "3 days"
func formatDuration(d time.Duration) string {
	if d < time.Minute {
		return d.Round(time.Second).String()
	}
	if d < time.Hour {
		minutes := int(d.Minutes())
		return formatTimeUnit(minutes, "minute")
	}
	if d < 24*time.Hour {
		hours := int(d.Hours())
		return formatTimeUnit(hours, "hour")
	}
	days := int(d.Hours() / 24)
	hours := int(d.Hours()) % 24
	if hours > 0 {
		return formatTimeUnit(days, "day") + " " + formatTimeUnit(hours, "hour")
	}
	return formatTimeUnit(days, "day")
}

// formatTimeUnit formats a time value with proper singular/plural unit names.
//
// Examples:
//   - formatTimeUnit(1, "hour") → "1 hour"
//   - formatTimeUnit(5, "hour") → "5 hours"
//   - formatTimeUnit(1, "day") → "1 day"
//   - formatTimeUnit(30, "day") → "30 days"
func formatTimeUnit(value int, unit string) string {
	if value == 1 {
		return "1 " + unit
	}
	return fmt.Sprintf("%d %ss", value, unit)
}
