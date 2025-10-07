package gossip

// ClusterStatusProvider provides cluster-wide status information for monitoring
// and admin interfaces. Implementations return the operational status of all
// nodes in the cluster, including queue depths, active workers, and uptime.
type ClusterStatusProvider interface {
	// GetClusterStatus returns a map of node IDs to their current status.
	GetClusterStatus() map[string]NodeStatus
}

// Ensure Gossip implements ClusterStatusProvider at compile time.
var _ ClusterStatusProvider = (*Gossip)(nil)
