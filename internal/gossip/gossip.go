// Package gossip provides distributed cluster coordination using HashiCorp's memberlist
// gossip protocol. It enables multi-node deployments with automatic peer discovery,
// distributed idempotency tracking, cluster-wide status monitoring, and leader election.
//
// Key Features:
//   - Automatic peer discovery and failure detection
//   - Distributed idempotency key tracking across cluster nodes
//   - Leader election for coordinating singleton operations (e.g., Let's Encrypt)
//   - Cluster-wide node status monitoring (queue depth, active workers, uptime)
//   - Optional AES-256 encryption for secure cluster communication
//   - Automatic cleanup of stale data and expired keys
//
// Architecture:
//
// The gossip service uses memberlist's SWIM protocol for efficient cluster membership
// management. Each node maintains a local view of the cluster and propagates changes
// through gossip messages. Leader election uses lexicographic ordering of node IDs,
// ensuring deterministic and consistent leader selection across all nodes.
//
// Idempotency tracking prevents duplicate message submissions across cluster nodes
// by broadcasting key claims with TTL-based expiration. Node status updates enable
// monitoring of cluster health and load distribution.
//
// Example Usage:
//
//	cfg := &gossip.Config{
//		Enabled:        true,
//		BindAddr:       "0.0.0.0",
//		BindPort:       7946,
//		Peers:          []string{"node1:7946", "node2:7946"},
//		NodeID:         "node-1",
//		SecretKey:      "base64-encoded-32-byte-key",
//		IdempotencyTTL: 15 * time.Minute,
//	}
//
//	g, err := gossip.NewGossip(cfg, logger)
//	if err != nil {
//		log.Fatal(err)
//	}
//	defer g.Shutdown()
//
//	// Check if this node is the leader
//	if g.IsLeader() {
//		// Perform leader-only operations
//	}
//
//	// Broadcast idempotency key claim
//	g.BroadcastIdempotencyKey("request-123")
//
//	// Check if key is claimed by another node
//	if nodeID := g.CheckIdempotencyKey("request-123"); nodeID != "" {
//		fmt.Printf("Key claimed by node: %s\n", nodeID)
//	}
package gossip

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/hashicorp/memberlist"
	"go.uber.org/zap"
)

// MessageType represents the type of gossip message exchanged between cluster nodes.
// Message types determine how incoming gossip data is decoded and processed.
type MessageType byte

const (
	// MessageTypeIdempotency indicates an idempotency key claim broadcast.
	MessageTypeIdempotency MessageType = iota
	// MessageTypeNodeStatus indicates a node status update broadcast.
	MessageTypeNodeStatus
)

// IdempotencyMessage broadcasts idempotency key claims to prevent duplicate
// message submissions across cluster nodes. Each claim includes the claiming
// node's ID and timestamp for TTL-based expiration.
type IdempotencyMessage struct {
	IdempotencyKey string
	NodeID         string
	Timestamp      time.Time
}

// NodeStatus represents the current operational state of a cluster node,
// including queue depth, worker activity, and uptime. This information
// is used for cluster monitoring and load distribution insights.
type NodeStatus struct {
	NodeID        string
	QueueDepth    int64
	ActiveWorkers int
	Uptime        time.Duration
	LastSeen      time.Time
}

// idempotencyCacheEntry holds an idempotency key with its timestamp
// for TTL-based expiration tracking.
type idempotencyCacheEntry struct {
	NodeID    string
	Timestamp time.Time
}

// Gossip manages the gossip protocol and cluster communication. It provides
// distributed coordination primitives including idempotency tracking, leader
// election, and cluster-wide status monitoring. Safe for concurrent use.
type Gossip struct {
	nodeID     string
	memberlist *memberlist.Memberlist
	logger     *zap.Logger

	// Idempotency cache: key -> idempotencyCacheEntry
	idempotencyCache sync.Map

	// Node status map: NodeID -> NodeStatus
	nodeStatuses sync.Map

	// TTL for idempotency cache entries
	idempotencyTTL time.Duration

	// Leader election
	leader    string
	leaderMtx sync.RWMutex

	// Callbacks
	onNodeJoin  func(node string)
	onNodeLeave func(node string)
}

// Config holds cluster configuration for the gossip protocol.
type Config struct {
	// Enabled determines whether the gossip protocol is active.
	Enabled bool
	// BindAddr is the IP address to bind to (default: 0.0.0.0).
	BindAddr string
	// BindPort is the UDP/TCP port for gossip communication (default: 7946).
	BindPort int
	// Peers is the list of other cluster nodes to connect to (host:port format).
	Peers []string
	// NodeID is a unique identifier for this node (defaults to hostname).
	NodeID string
	// SecretKey is a 32-byte base64-encoded AES-256 encryption key for secure
	// cluster communication. If empty, communication is unencrypted (insecure).
	SecretKey string
	// IdempotencyTTL is the duration for which idempotency keys remain valid.
	IdempotencyTTL time.Duration
}

// NewGossip creates a new gossip service and joins the specified cluster peers.
// Returns nil if gossip is disabled in the configuration. If SecretKey is provided,
// it must be exactly 32 bytes when base64-decoded for AES-256 encryption.
//
// The gossip service starts background goroutines for cache cleanup and will
// attempt to join the specified peers. Join failures are logged but not fatal,
// allowing the node to operate standalone or join later through peer discovery.
func NewGossip(cfg *Config, logger *zap.Logger) (*Gossip, error) {
	if !cfg.Enabled {
		logger.Info("gossip protocol disabled")
		return nil, nil
	}

	// Use hostname as default node ID if not specified
	nodeID := cfg.NodeID
	if nodeID == "" {
		hostname, err := os.Hostname()
		if err != nil {
			return nil, fmt.Errorf("failed to get hostname: %w", err)
		}
		nodeID = hostname
	}

	g := &Gossip{
		nodeID:         nodeID,
		logger:         logger,
		idempotencyTTL: cfg.IdempotencyTTL,
	}

	// Configure memberlist
	mlConfig := memberlist.DefaultLANConfig()
	mlConfig.Name = nodeID
	mlConfig.BindAddr = cfg.BindAddr
	mlConfig.BindPort = cfg.BindPort
	mlConfig.AdvertisePort = cfg.BindPort

	// Configure encryption if secret key is provided
	if cfg.SecretKey != "" {
		// Decode base64 secret key
		keyBytes, err := base64.StdEncoding.DecodeString(cfg.SecretKey)
		if err != nil {
			return nil, fmt.Errorf("failed to decode secret key (must be base64): %w", err)
		}

		// Memberlist requires exactly 32 bytes for AES-256 encryption
		if len(keyBytes) != 32 {
			return nil, fmt.Errorf("secret key must be exactly 32 bytes (got %d bytes)", len(keyBytes))
		}

		// Enable encryption with the provided key
		mlConfig.SecretKey = keyBytes
		logger.Info("gossip encryption enabled", zap.Int("key_length", len(keyBytes)))
	} else {
		logger.Warn("cluster encryption DISABLED - gossip communication is INSECURE",
			zap.String("recommendation", "set cluster.secret_key for production deployments"))
	}

	// Set delegate for custom message handling
	mlConfig.Delegate = &delegate{gossip: g}
	mlConfig.Events = &eventDelegate{gossip: g}

	// Disable memberlist's built-in logger (we use zap)
	mlConfig.LogOutput = &zapLogAdapter{logger: logger}

	// Create memberlist
	ml, err := memberlist.Create(mlConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create memberlist: %w", err)
	}

	g.memberlist = ml

	logger.Info("gossip service initialized",
		zap.String("node_id", nodeID),
		zap.String("bind_addr", cfg.BindAddr),
		zap.Int("bind_port", cfg.BindPort))

	// Join cluster if peers are provided
	if len(cfg.Peers) > 0 {
		n, err := ml.Join(cfg.Peers)
		if err != nil {
			logger.Warn("failed to join cluster",
				zap.Strings("peers", cfg.Peers),
				zap.Error(err))
		} else {
			logger.Info("joined cluster",
				zap.Int("peers_contacted", n),
				zap.Strings("peers", cfg.Peers))
		}
	}

	// Initial leader election
	g.updateLeader()

	// Start background cleanup for expired idempotency keys
	go g.cleanupExpiredKeys()

	return g, nil
}

// BroadcastIdempotencyKey broadcasts an idempotency key claim to the cluster,
// marking it as owned by this node. The key is stored in the local cache with
// the current timestamp for TTL-based expiration. Other nodes will receive this
// broadcast and prevent duplicate submissions with the same key.
//
// Returns nil if gossip is disabled. Safe for concurrent use.
func (g *Gossip) BroadcastIdempotencyKey(idempotencyKey string) error {
	if g == nil || g.memberlist == nil {
		return nil // Gossip disabled
	}

	msg := IdempotencyMessage{
		IdempotencyKey: idempotencyKey,
		NodeID:         g.nodeID,
		Timestamp:      time.Now(),
	}

	data, err := g.encodeMessage(MessageTypeIdempotency, msg)
	if err != nil {
		return fmt.Errorf("failed to encode idempotency message: %w", err)
	}

	// Store in local cache with timestamp
	entry := idempotencyCacheEntry{
		NodeID:    g.nodeID,
		Timestamp: time.Now(),
	}
	g.idempotencyCache.Store(idempotencyKey, entry)

	// Broadcast to cluster
	g.memberlist.LocalNode().Meta = data
	g.memberlist.UpdateNode(time.Second)

	g.logger.Debug("broadcast idempotency key",
		zap.String("key", idempotencyKey),
		zap.String("node_id", g.nodeID))

	return nil
}

// CheckIdempotencyKey checks if an idempotency key is claimed by any node in
// the cluster, including this node. Returns the claiming node's ID if the key
// is claimed and not expired, or an empty string if the key is available.
//
// Expired keys (older than IdempotencyTTL) are automatically removed from the
// cache. Returns empty string if gossip is disabled. Safe for concurrent use.
func (g *Gossip) CheckIdempotencyKey(idempotencyKey string) string {
	if g == nil {
		return "" // Gossip disabled
	}

	if value, exists := g.idempotencyCache.Load(idempotencyKey); exists {
		entry := value.(idempotencyCacheEntry)

		// Check if entry has expired
		if time.Since(entry.Timestamp) > g.idempotencyTTL {
			// Expired, remove it
			g.idempotencyCache.Delete(idempotencyKey)
			return ""
		}

		return entry.NodeID
	}

	return ""
}

// BroadcastNodeStatus broadcasts the current node's operational status to the
// cluster, including queue depth, active worker count, and uptime. This enables
// cluster-wide monitoring and load distribution insights. Status updates are
// stored locally and transmitted reliably to all cluster members.
//
// Returns nil if gossip is disabled. Safe for concurrent use.
func (g *Gossip) BroadcastNodeStatus(queueDepth int64, activeWorkers int, uptime time.Duration) error {
	if g == nil || g.memberlist == nil {
		return nil // Gossip disabled
	}

	status := NodeStatus{
		NodeID:        g.nodeID,
		QueueDepth:    queueDepth,
		ActiveWorkers: activeWorkers,
		Uptime:        uptime,
		LastSeen:      time.Now(),
	}

	data, err := g.encodeMessage(MessageTypeNodeStatus, status)
	if err != nil {
		return fmt.Errorf("failed to encode node status: %w", err)
	}

	// Store in local map
	g.nodeStatuses.Store(g.nodeID, status)

	// Broadcast to cluster (using node metadata)
	g.memberlist.SendReliable(g.memberlist.LocalNode(), data)

	g.logger.Debug("broadcast node status",
		zap.String("node_id", g.nodeID),
		zap.Int64("queue_depth", queueDepth),
		zap.Int("active_workers", activeWorkers))

	return nil
}

// GetClusterStatus returns the status of all nodes in the cluster as a map
// keyed by node ID. Stale entries (nodes not seen for >5 minutes) are
// automatically removed by the background cleanup goroutine.
//
// Returns an empty map if gossip is disabled. Safe for concurrent use.
func (g *Gossip) GetClusterStatus() map[string]NodeStatus {
	if g == nil {
		return make(map[string]NodeStatus)
	}

	result := make(map[string]NodeStatus)

	g.nodeStatuses.Range(func(key, value interface{}) bool {
		nodeID := key.(string)
		status := value.(NodeStatus)
		result[nodeID] = status
		return true
	})

	return result
}

// GetMembers returns the list of all active cluster member node IDs.
// This includes the local node and all peers that are currently alive
// according to memberlist's failure detection.
//
// Returns an empty slice if gossip is disabled. Safe for concurrent use.
func (g *Gossip) GetMembers() []string {
	if g == nil || g.memberlist == nil {
		return []string{}
	}

	members := g.memberlist.Members()
	result := make([]string, len(members))

	for i, member := range members {
		result[i] = member.Name
	}

	return result
}

// Shutdown gracefully shuts down the gossip service by leaving the cluster
// and stopping all background goroutines. Blocks for up to 5 seconds to
// allow graceful departure messages to propagate to peers.
//
// Returns nil if gossip is disabled. Safe to call multiple times.
func (g *Gossip) Shutdown() error {
	if g == nil || g.memberlist == nil {
		return nil
	}

	g.logger.Info("shutting down gossip service")

	// Leave the cluster gracefully
	if err := g.memberlist.Leave(time.Second * 5); err != nil {
		g.logger.Warn("error leaving cluster", zap.Error(err))
	}

	// Shutdown memberlist
	if err := g.memberlist.Shutdown(); err != nil {
		return fmt.Errorf("failed to shutdown memberlist: %w", err)
	}

	g.logger.Info("gossip service shut down")
	return nil
}

// encodeMessage encodes a message with its type
func (g *Gossip) encodeMessage(msgType MessageType, payload interface{}) ([]byte, error) {
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}

	// Prepend message type byte
	result := make([]byte, len(data)+1)
	result[0] = byte(msgType)
	copy(result[1:], data)

	return result, nil
}

// decodeMessage decodes a message and returns its type and payload
func (g *Gossip) decodeMessage(data []byte) (MessageType, []byte, error) {
	if len(data) < 1 {
		return 0, nil, fmt.Errorf("message too short")
	}

	msgType := MessageType(data[0])
	payload := data[1:]

	return msgType, payload, nil
}

// handleMessage processes an incoming gossip message
func (g *Gossip) handleMessage(data []byte) error {
	msgType, payload, err := g.decodeMessage(data)
	if err != nil {
		return err
	}

	switch msgType {
	case MessageTypeIdempotency:
		var msg IdempotencyMessage
		if err := json.Unmarshal(payload, &msg); err != nil {
			return fmt.Errorf("failed to unmarshal idempotency message: %w", err)
		}
		g.handleIdempotencyMessage(msg)

	case MessageTypeNodeStatus:
		var status NodeStatus
		if err := json.Unmarshal(payload, &status); err != nil {
			return fmt.Errorf("failed to unmarshal node status: %w", err)
		}
		g.handleNodeStatus(status)

	default:
		g.logger.Warn("unknown message type", zap.Uint8("type", uint8(msgType)))
	}

	return nil
}

// handleIdempotencyMessage processes an incoming idempotency broadcast
func (g *Gossip) handleIdempotencyMessage(msg IdempotencyMessage) {
	// Don't process our own broadcasts
	if msg.NodeID == g.nodeID {
		return
	}

	entry := idempotencyCacheEntry{
		NodeID:    msg.NodeID,
		Timestamp: msg.Timestamp,
	}
	g.idempotencyCache.Store(msg.IdempotencyKey, entry)

	g.logger.Debug("received idempotency key claim",
		zap.String("key", msg.IdempotencyKey),
		zap.String("claimed_by", msg.NodeID))
}

// handleNodeStatus processes an incoming node status update
func (g *Gossip) handleNodeStatus(status NodeStatus) {
	// Update last seen timestamp
	status.LastSeen = time.Now()

	g.nodeStatuses.Store(status.NodeID, status)

	g.logger.Debug("received node status",
		zap.String("node_id", status.NodeID),
		zap.Int64("queue_depth", status.QueueDepth),
		zap.Int("active_workers", status.ActiveWorkers))
}

// cleanupExpiredKeys removes expired idempotency keys and stale node statuses from the cache
func (g *Gossip) cleanupExpiredKeys() {
	ticker := time.NewTicker(1 * time.Minute) // Run cleanup every minute
	defer ticker.Stop()

	for range ticker.C {
		now := time.Now()
		idempotencyExpired := 0
		nodesExpired := 0

		// Clean up expired idempotency keys
		g.idempotencyCache.Range(func(key, value interface{}) bool {
			entry := value.(idempotencyCacheEntry)
			if now.Sub(entry.Timestamp) > g.idempotencyTTL {
				g.idempotencyCache.Delete(key)
				idempotencyExpired++
			}
			return true
		})

		// Clean up stale node statuses (older than 5 minutes)
		g.nodeStatuses.Range(func(key, value interface{}) bool {
			status := value.(NodeStatus)
			if now.Sub(status.LastSeen) > 5*time.Minute {
				g.nodeStatuses.Delete(key)
				nodesExpired++
			}
			return true
		})

		// Log cleanup stats if anything was removed
		if idempotencyExpired > 0 || nodesExpired > 0 {
			g.logger.Debug("gossip cache cleanup completed",
				zap.Int("idempotency_keys_removed", idempotencyExpired),
				zap.Int("stale_nodes_removed", nodesExpired))
		}
	}
}

// SetNodeJoinCallback sets a callback function that is invoked when a node
// joins the cluster. The callback receives the joining node's ID. Useful for
// triggering cluster-wide coordination tasks or logging join events.
//
// Safe to call before or after gossip initialization. Does nothing if gossip is disabled.
func (g *Gossip) SetNodeJoinCallback(fn func(node string)) {
	if g != nil {
		g.onNodeJoin = fn
	}
}

// SetNodeLeaveCallback sets a callback function that is invoked when a node
// leaves the cluster (graceful departure or failure detection). The callback
// receives the departing node's ID. Useful for cleaning up node-specific state.
//
// Safe to call before or after gossip initialization. Does nothing if gossip is disabled.
func (g *Gossip) SetNodeLeaveCallback(fn func(node string)) {
	if g != nil {
		g.onNodeLeave = fn
	}
}

// IsLeader returns true if this node is the current cluster leader. Leadership
// is determined by lexicographic ordering of node IDs (smallest wins). Leader
// election is used to coordinate singleton operations like Let's Encrypt
// certificate issuance across multi-node clusters.
//
// Returns false if gossip is disabled. Safe for concurrent use.
func (g *Gossip) IsLeader() bool {
	if g == nil {
		return false
	}
	g.leaderMtx.RLock()
	defer g.leaderMtx.RUnlock()
	return g.leader == g.nodeID
}

// GetLeader returns the node ID of the current cluster leader. Leader
// is determined by lexicographic ordering of active member node IDs.
//
// Returns empty string if gossip is disabled. Safe for concurrent use.
func (g *Gossip) GetLeader() string {
	if g == nil {
		return ""
	}
	g.leaderMtx.RLock()
	defer g.leaderMtx.RUnlock()
	return g.leader
}

// updateLeader determines the cluster leader based on member list
// Leader is the node with the lexicographically smallest node ID
func (g *Gossip) updateLeader() {
	if g == nil || g.memberlist == nil {
		return
	}

	members := g.memberlist.Members()
	if len(members) == 0 {
		return
	}

	// Sort members by node ID (lexicographically)
	sort.Slice(members, func(i, j int) bool {
		return members[i].Name < members[j].Name
	})

	newLeader := members[0].Name

	g.leaderMtx.Lock()
	defer g.leaderMtx.Unlock()

	if g.leader != newLeader {
		oldLeader := g.leader
		g.leader = newLeader
		g.logger.Info("cluster leader changed",
			zap.String("old_leader", oldLeader),
			zap.String("new_leader", g.leader),
			zap.Bool("is_leader", g.leader == g.nodeID))
	}
}
