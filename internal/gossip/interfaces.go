package gossip

// ClusterStatusProvider provides cluster-wide status information
type ClusterStatusProvider interface {
	GetClusterStatus() map[string]NodeStatus
}

// Ensure Gossip implements ClusterStatusProvider
var _ ClusterStatusProvider = (*Gossip)(nil)
