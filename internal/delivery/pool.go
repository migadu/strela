package delivery

import (
	"fmt"
	"sync"
	"time"

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
}

// NewConnectionPool creates a new connection pool.
func NewConnectionPool(ttlSeconds int) *ConnectionPool {
	if ttlSeconds <= 0 {
		ttlSeconds = 5 // Default safe TTL
	}
	return &ConnectionPool{
		connections: make(map[string][]*PooledConnection),
		ttl:         time.Duration(ttlSeconds) * time.Second,
		maxIdle:     10, // Per-key limit
	}
}

// poolKey generates a unique key for the connection pool.
func poolKey(mxHost, sourceIP string) string {
	return fmt.Sprintf("%s|%s", mxHost, sourceIP)
}

// Get retrieves an idle connection from the pool if available.
// Returns nil if no valid connection is found.
func (p *ConnectionPool) Get(mxHost, sourceIP string) *smtp.Client {
	p.mu.Lock()
	defer p.mu.Unlock()

	key := poolKey(mxHost, sourceIP)
	conns := p.connections[key]

	// Iterate backwards to get most recent (LIFO) - actually standard slice pop is fine
	// But we need to filter expired ones.
	now := time.Now()

	for len(conns) > 0 {
		// Pop last
		lastIdx := len(conns) - 1
		conn := conns[lastIdx]
		conns[lastIdx] = nil // Avoid leak
		conns = conns[:lastIdx]

		// Update map
		p.connections[key] = conns

		// Check expiry
		if now.Sub(conn.timestamp) > p.ttl {
			// Expired, close it and try next
			conn.client.Close() // Best effort close
			continue
		}

		// Verify connection is still alive with NOOP
		if err := conn.client.Noop(); err != nil {
			conn.client.Close()
			continue
		}

		return conn.client
	}

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

// CloseAll closes all idle connections in the pool.
func (p *ConnectionPool) CloseAll() {
	p.mu.Lock()
	defer p.mu.Unlock()

	for key, conns := range p.connections {
		for _, conn := range conns {
			conn.client.Close()
		}
		delete(p.connections, key)
	}
}
