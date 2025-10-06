package handler

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"fune/internal/gossip"

	"go.uber.org/zap"
)

// ClusterStatusHandler provides cluster-wide status information
type ClusterStatusHandler struct {
	statusProvider *gossip.Gossip
	logger         *zap.Logger
}

// NewClusterStatusHandler creates a new cluster status handler
func NewClusterStatusHandler(g *gossip.Gossip, logger *zap.Logger) *ClusterStatusHandler {
	return &ClusterStatusHandler{
		statusProvider: g,
		logger:         logger,
	}
}

// ClusterNodeStatus represents the status of a node in the cluster
type ClusterNodeStatus struct {
	NodeID        string `json:"node_id"`
	QueueDepth    int64  `json:"queue_depth"`
	ActiveWorkers int    `json:"active_workers"`
	Uptime        string `json:"uptime"`
	LastSeen      string `json:"last_seen"`
	UptimeSeconds int64  `json:"uptime_seconds"`
}

// ClusterStatusResponse represents the cluster status API response
type ClusterStatusResponse struct {
	Timestamp string                       `json:"timestamp"`
	Nodes     map[string]ClusterNodeStatus `json:"nodes"`
	NodeCount int                          `json:"node_count"`
}

// ServeHTTP handles cluster status requests
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

// formatDuration formats a duration in a human-readable format
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

func formatTimeUnit(value int, unit string) string {
	if value == 1 {
		return "1 " + unit
	}
	return fmt.Sprintf("%d %ss", value, unit)
}
