package gossip

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/hashicorp/memberlist"
	"go.uber.org/zap"
)

// MessageType represents the type of gossip message
type MessageType byte

const (
	MessageTypeIdempotency MessageType = iota
	MessageTypeNodeStatus
)

// IdempotencyMessage broadcasts idempotency key claims
type IdempotencyMessage struct {
	IdempotencyKey string
	NodeID         string
	Timestamp      time.Time
}

// NodeStatus represents the current state of a node
type NodeStatus struct {
	NodeID        string
	QueueDepth    int64
	ActiveWorkers int
	Uptime        time.Duration
	LastSeen      time.Time
}

// idempotencyCacheEntry holds an idempotency key with its timestamp
type idempotencyCacheEntry struct {
	NodeID    string
	Timestamp time.Time
}

// Gossip manages the gossip protocol and cluster communication
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

	// Callbacks
	onNodeJoin  func(node string)
	onNodeLeave func(node string)
}

// Config holds gossip configuration
type Config struct {
	Enabled        bool
	BindPort       int
	JoinAddresses  []string
	NodeID         string
	IdempotencyTTL time.Duration
}

// NewGossip creates a new gossip service
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
	mlConfig.BindPort = cfg.BindPort
	mlConfig.AdvertisePort = cfg.BindPort

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
		zap.Int("bind_port", cfg.BindPort))

	// Join cluster if seed nodes are provided
	if len(cfg.JoinAddresses) > 0 {
		n, err := ml.Join(cfg.JoinAddresses)
		if err != nil {
			logger.Warn("failed to join cluster",
				zap.Strings("seed_nodes", cfg.JoinAddresses),
				zap.Error(err))
		} else {
			logger.Info("joined gossip cluster",
				zap.Int("nodes_contacted", n),
				zap.Strings("seed_nodes", cfg.JoinAddresses))
		}
	}

	// Start background cleanup for expired idempotency keys
	go g.cleanupExpiredKeys()

	return g, nil
}

// BroadcastIdempotencyKey broadcasts an idempotency key claim to the cluster
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

// CheckIdempotencyKey checks if an idempotency key is claimed by another node
// Returns the node ID if claimed, empty string if available
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

// BroadcastNodeStatus broadcasts the current node's status to the cluster
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

// GetClusterStatus returns the status of all nodes in the cluster
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

// GetMembers returns the list of cluster members
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

// Shutdown gracefully shuts down the gossip service
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

// SetNodeJoinCallback sets a callback for when nodes join
func (g *Gossip) SetNodeJoinCallback(fn func(node string)) {
	if g != nil {
		g.onNodeJoin = fn
	}
}

// SetNodeLeaveCallback sets a callback for when nodes leave
func (g *Gossip) SetNodeLeaveCallback(fn func(node string)) {
	if g != nil {
		g.onNodeLeave = fn
	}
}
