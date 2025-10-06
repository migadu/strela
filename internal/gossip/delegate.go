package gossip

import (
	"github.com/hashicorp/memberlist"
	"go.uber.org/zap"
)

// delegate implements memberlist.Delegate for custom message handling
type delegate struct {
	gossip *Gossip
}

// NodeMeta returns metadata for this node
func (d *delegate) NodeMeta(limit int) []byte {
	// Return empty metadata (we use broadcasts instead)
	return []byte{}
}

// NotifyMsg is called when a user message is received
func (d *delegate) NotifyMsg(msg []byte) {
	if err := d.gossip.handleMessage(msg); err != nil {
		d.gossip.logger.Error("failed to handle gossip message", zap.Error(err))
	}
}

// GetBroadcasts returns messages to broadcast to the cluster
func (d *delegate) GetBroadcasts(overhead, limit int) [][]byte {
	// We handle broadcasts manually, so return empty
	return [][]byte{}
}

// LocalState returns the local state for synchronization
func (d *delegate) LocalState(join bool) []byte {
	// Not used in our implementation
	return []byte{}
}

// MergeRemoteState merges remote state into local state
func (d *delegate) MergeRemoteState(buf []byte, join bool) {
	// Not used in our implementation
}

// eventDelegate implements memberlist.EventDelegate for cluster events
type eventDelegate struct {
	gossip *Gossip
}

// NotifyJoin is called when a node joins the cluster
func (e *eventDelegate) NotifyJoin(node *memberlist.Node) {
	e.gossip.logger.Info("node joined cluster",
		zap.String("node_id", node.Name),
		zap.String("addr", node.Address()))

	// Re-evaluate leader
	e.gossip.updateLeader()

	if e.gossip.onNodeJoin != nil {
		e.gossip.onNodeJoin(node.Name)
	}
}

// NotifyLeave is called when a node leaves the cluster
func (e *eventDelegate) NotifyLeave(node *memberlist.Node) {
	e.gossip.logger.Info("node left cluster",
		zap.String("node_id", node.Name),
		zap.String("addr", node.Address()))

	// Remove node status
	e.gossip.nodeStatuses.Delete(node.Name)

	// Re-evaluate leader
	e.gossip.updateLeader()

	if e.gossip.onNodeLeave != nil {
		e.gossip.onNodeLeave(node.Name)
	}
}

// NotifyUpdate is called when a node updates its metadata
func (e *eventDelegate) NotifyUpdate(node *memberlist.Node) {
	e.gossip.logger.Debug("node updated",
		zap.String("node_id", node.Name))
}
