package delivery

import (
	"fmt"
	"log/slog"
	"sync"
	"time"

	"strela/internal/recovery"

	"github.com/emersion/go-smtp"
)

// PooledConnection wraps an SMTP client with metadata.
type PooledConnection struct {
	client    *smtp.Client
	mxHost    string
	sourceIP  string
	timestamp time.Time
}

// ConnectionPool manages reusable SMTP connections.
type ConnectionPool struct {
	mu          sync.Mutex
	connections map[string][]*PooledConnection // Key: "mxHost|sourceIP"
	ttl         time.Duration
	maxIdle     int
	logger      *slog.Logger
	stopCh      chan struct{}
}

// NewConnectionPool creates a new connection pool with background cleanup.
func NewConnectionPool(ttlSeconds int, logger *slog.Logger) *ConnectionPool {
	if ttlSeconds <= 0 {
		ttlSeconds = 5 // Default safe TTL
	}
	if logger == nil {
		logger = slog.Default()
	}

	p := &ConnectionPool{
		connections: make(map[string][]*PooledConnection),
		ttl:         time.Duration(ttlSeconds) * time.Second,
		maxIdle:     10, // Per-key limit
		logger:      logger,
		stopCh:      make(chan struct{}),
	}

	// Start background cleanup goroutine
	p.startCleanup()

	return p
}

// startCleanup starts a background goroutine to periodically clean up expired connections.
func (p *ConnectionPool) startCleanup() {
	recovery.SafeGo(p.logger, "connection-pool-cleanup", func() {
		// Run cleanup every TTL interval
		ticker := time.NewTicker(p.ttl)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				p.cleanupExpired()
			case <-p.stopCh:
				return
			}
		}
	})
}

// cleanupExpired removes all expired connections from the pool.
func (p *ConnectionPool) cleanupExpired() {
	p.mu.Lock()
	defer p.mu.Unlock()

	now := time.Now()
	removed := 0

	for key, conns := range p.connections {
		var active []*PooledConnection
		for _, conn := range conns {
			if now.Sub(conn.timestamp) > p.ttl {
				// Expired, close it
				conn.client.Close()
				removed++
			} else {
				active = append(active, conn)
			}
		}

		if len(active) == 0 {
			delete(p.connections, key)
		} else {
			p.connections[key] = active
		}
	}

	if removed > 0 {
		p.logger.Debug("cleaned up expired pooled connections", "removed", removed)
	}
}

// poolKey generates a unique key for the connection pool.
func poolKey(mxHost, sourceIP string) string {
	return fmt.Sprintf("%s|%s", mxHost, sourceIP)
}

// Get retrieves an idle connection from the pool if available.
// Returns nil if no valid connection is found.
func (p *ConnectionPool) Get(mxHost, sourceIP string) *smtp.Client {
	key := poolKey(mxHost, sourceIP)

	for {
		candidate := p.popCandidate(key, mxHost, sourceIP)
		if candidate == nil {
			return nil
		}

		// Verify connection is still alive with NOOP (outside the lock to avoid
		// blocking all pool operations during a network round-trip)
		if err := candidate.Noop(); err != nil {
			p.logger.Debug("pooled connection no longer alive", "mx", mxHost, "source_ip", sourceIP, "error", err)
			candidate.Close()
			continue
		}

		p.logger.Debug("connection pool hit", "mx", mxHost, "source_ip", sourceIP)
		return candidate
	}
}

// popCandidate removes and returns the most recent non-expired connection from the pool.
// Returns nil if no valid candidate is found.
func (p *ConnectionPool) popCandidate(key, mxHost, sourceIP string) *smtp.Client {
	p.mu.Lock()
	defer p.mu.Unlock()

	conns := p.connections[key]
	now := time.Now()

	for len(conns) > 0 {
		// Pop last (LIFO - most recent)
		lastIdx := len(conns) - 1
		conn := conns[lastIdx]
		conns[lastIdx] = nil // Avoid leak
		conns = conns[:lastIdx]
		p.connections[key] = conns

		// Check expiry
		if now.Sub(conn.timestamp) > p.ttl {
			p.logger.Debug("closing expired pooled connection", "mx", mxHost, "source_ip", sourceIP, "age", now.Sub(conn.timestamp))
			conn.client.Close()
			continue
		}

		return conn.client
	}

	p.logger.Debug("connection pool miss", "mx", mxHost, "source_ip", sourceIP)
	return nil
}

// Put returns a connection to the pool.
// The caller must assume the connection is reset (RSET called).
func (p *ConnectionPool) Put(client *smtp.Client, mxHost, sourceIP string) {
	if client == nil {
		return
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	key := poolKey(mxHost, sourceIP)
	p.logger.Debug("returning connection to pool", "mx", mxHost, "source_ip", sourceIP)

	conns := p.connections[key]

	// Enforce max idle limit
	if len(conns) >= p.maxIdle {
		client.Close() // Too many idle, close it
		return
	}

	p.connections[key] = append(conns, &PooledConnection{
		client:    client,
		mxHost:    mxHost,
		sourceIP:  sourceIP,
		timestamp: time.Now(),
	})
}

// CloseAll closes all idle connections in the pool and stops the cleanup goroutine.
func (p *ConnectionPool) CloseAll() {
	// Signal cleanup goroutine to stop and wait for it to exit before
	// acquiring the mutex, avoiding a race with cleanupExpired().
	close(p.stopCh)

	// Give the cleanup goroutine time to notice stopCh and release the lock.
	// cleanupExpired holds mu briefly; this ensures we don't race with it.
	time.Sleep(100 * time.Millisecond)

	p.mu.Lock()
	defer p.mu.Unlock()

	count := 0
	for key, conns := range p.connections {
		for _, conn := range conns {
			conn.client.Close()
			count++
		}
		delete(p.connections, key)
	}

	if count > 0 {
		p.logger.Info("closed all pooled connections", "count", count)
	}
}

// Stats returns the current pool statistics.
func (p *ConnectionPool) Stats() (totalConnections int, uniqueKeys int) {
	p.mu.Lock()
	defer p.mu.Unlock()

	for _, conns := range p.connections {
		totalConnections += len(conns)
	}
	uniqueKeys = len(p.connections)
	return
}
