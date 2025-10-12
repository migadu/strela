package gossip

import (
	"github.com/hashicorp/memberlist"
)

// delegate implements memberlist.Delegate for custom message handling and state
// synchronization. It bridges memberlist's callback interface to our gossip service's
// message processing logic.
type delegate struct {
	gossip *Gossip
}

// NodeMeta returns metadata for this node that will be gossiped to other members.
// We use explicit broadcasts instead of metadata, so this returns empty bytes.
func (d *delegate) NodeMeta(limit int) []byte {
	// Return empty metadata (we use broadcasts instead)
	return []byte{}
}

// NotifyMsg is called when a user-defined message is received from another
// cluster member. Messages are decoded and routed to appropriate handlers
// based on their MessageType (idempotency claims, node status updates, etc.).
func (d *delegate) NotifyMsg(msg []byte) {
	if err := d.gossip.handleMessage(msg); err != nil {
		d.gossip.logger.Error("failed to handle gossip message", "error", err)
	}
}

// GetBroadcasts returns messages to broadcast to the cluster. We handle
// broadcasts manually through SendReliable and UpdateNode, so this returns empty.
func (d *delegate) GetBroadcasts(overhead, limit int) [][]byte {
	// We handle broadcasts manually, so return empty
	return [][]byte{}
}

// LocalState returns the local state for anti-entropy synchronization during
// node joins. Not used in our implementation as we rely on explicit broadcasts.
func (d *delegate) LocalState(join bool) []byte {
	// Not used in our implementation
	return []byte{}
}

// MergeRemoteState merges remote state into local state during anti-entropy
// synchronization. Not used in our implementation as we rely on explicit broadcasts.
func (d *delegate) MergeRemoteState(buf []byte, join bool) {
	// Not used in our implementation
}

// eventDelegate implements memberlist.EventDelegate for cluster membership events
// (node joins, leaves, updates). It triggers leader re-election and invokes
// user-defined callbacks for cluster topology changes.
type eventDelegate struct {
	gossip *Gossip
}

// NotifyJoin is called when a node joins the cluster. This triggers leader
// re-election and invokes the user-defined join callback if configured.
func (e *eventDelegate) NotifyJoin(node *memberlist.Node) {
	e.gossip.logger.Info("node joined cluster",
		"node_id", node.Name,
		"addr", node.Address())

	// Re-evaluate leader
	e.gossip.updateLeader()

	if e.gossip.onNodeJoin != nil {
		e.gossip.onNodeJoin(node.Name)
	}
}

// NotifyLeave is called when a node leaves the cluster (graceful departure
// or failure detection). This removes the node's status from the cache,
// triggers leader re-election, and invokes the user-defined leave callback.
func (e *eventDelegate) NotifyLeave(node *memberlist.Node) {
	e.gossip.logger.Info("node left cluster",
		"node_id", node.Name,
		"addr", node.Address())

	// Remove node status
	e.gossip.nodeStatuses.Delete(node.Name)

	// Re-evaluate leader
	e.gossip.updateLeader()

	if e.gossip.onNodeLeave != nil {
		e.gossip.onNodeLeave(node.Name)
	}
}

// NotifyUpdate is called when a node updates its metadata. Currently logged
// for debugging but does not trigger any state changes, as we use explicit
// broadcasts for node status updates instead of metadata propagation.
func (e *eventDelegate) NotifyUpdate(node *memberlist.Node) {
	e.gossip.logger.Debug("node updated",
		"node_id", node.Name)
}
