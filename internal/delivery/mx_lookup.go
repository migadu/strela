package delivery

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"fune/internal/config"
)

// MXLookup handles MX record lookups with in-memory caching.
// It uses a custom DNS resolver with configurable servers and stores results in a sync.Map.
type MXLookup struct {
	dnsResolver *DNSResolver
	logger      *slog.Logger
	cache       sync.Map // map[string]*mxCacheEntry
	cacheTTL    time.Duration
	negativeTTL time.Duration
	sf          singleflightGroup
}

// MXRecord represents a single MX record with hostname and priority.
type MXRecord struct {
	Host     string `json:"host"`
	Priority uint16 `json:"priority"`
}

// mxCacheEntry represents a cached MX lookup result (successful or failed).
type mxCacheEntry struct {
	records   []*MXRecord
	expiresAt time.Time
}

// NewMXLookup creates a new MX lookup service.
func NewMXLookup(dnsCfg *config.DNSConfig, logger *slog.Logger) *MXLookup {
	return &MXLookup{
		dnsResolver: NewDNSResolver(dnsCfg, logger),
		logger:      logger,
		cacheTTL:    time.Duration(dnsCfg.CacheTTLSeconds) * time.Second,
		negativeTTL: time.Duration(dnsCfg.CacheNegativeTTLSeconds) * time.Second,
	}
}

// Lookup performs MX record lookup for a domain with in-memory caching.
// It mitigates cache stampedes using singleflight.
func (m *MXLookup) Lookup(ctx context.Context, domain string) ([]*MXRecord, error) {
	// Try cache first
	if cached, ok := m.getFromCache(domain); ok {
		m.logger.Debug("MX cache hit", "domain", domain, "records", len(cached))
		return cached, nil
	}

	// Cache miss - join singleflight
	m.logger.Debug("MX cache miss, performing DNS lookup", "domain", domain)

	// Use context.Background() to ensure one client cancelling doesn't abort the lookup for others.
	// The resolver enforces its own timeout confguration.
	result, err := m.sf.do(domain, func() ([]*MXRecord, error) {
		m.logger.Debug("executing singleflight DNS lookup", "domain", domain)

		records, err := m.lookupDNS(context.Background(), domain)
		if err != nil {
			m.logger.Error("MX lookup failed", "domain", domain, "error", err)
			// Cache negative result
			m.storeInCache(domain, nil, m.negativeTTL)
			return nil, err
		}

		// Sort by priority (lower is higher priority)
		sort.Slice(records, func(i, j int) bool {
			return records[i].Priority < records[j].Priority
		})

		// Store in cache
		m.storeInCache(domain, records, m.cacheTTL)

		m.logger.Info("MX lookup successful", "domain", domain, "records", len(records))
		return records, nil
	})

	return result, err
}

// lookupDNS performs the actual DNS MX lookup.
func (m *MXLookup) lookupDNS(ctx context.Context, domain string) ([]*MXRecord, error) {
	mxRecords, err := m.dnsResolver.LookupMX(ctx, domain)
	if err != nil {
		return nil, fmt.Errorf("DNS lookup failed: %w", err)
	}

	if len(mxRecords) == 0 {
		return nil, fmt.Errorf("no MX records found for domain %s", domain)
	}

	records := make([]*MXRecord, len(mxRecords))
	for i, mx := range mxRecords {
		records[i] = &MXRecord{
			Host:     mx.Host,
			Priority: mx.Pref,
		}
	}

	return records, nil
}

// getFromCache retrieves and validates MX records from cache.
func (m *MXLookup) getFromCache(domain string) ([]*MXRecord, bool) {
	val, ok := m.cache.Load(domain)
	if !ok {
		return nil, false
	}

	entry, ok := val.(*mxCacheEntry)
	if !ok {
		m.logger.Error("type assertion failed for MX cache entry",
			"domain", domain,
			"type", fmt.Sprintf("%T", val))
		m.cache.Delete(domain) // Remove corrupted entry
		return nil, false
	}

	if time.Now().After(entry.expiresAt) {
		m.cache.Delete(domain)
		return nil, false
	}

	return entry.records, true
}

// storeInCache stores MX records in cache.
func (m *MXLookup) storeInCache(domain string, records []*MXRecord, ttl time.Duration) {
	m.cache.Store(domain, &mxCacheEntry{
		records:   records,
		expiresAt: time.Now().Add(ttl),
	})
}

// InvalidateCache removes a domain's MX records from the cache.
func (m *MXLookup) InvalidateCache(domain string) {
	m.cache.Delete(domain)
	m.logger.Debug("cache invalidated", "domain", domain)
}

// ReloadConfig updates DNS resolver configuration during a hot reload.
func (m *MXLookup) ReloadConfig(dnsCfg *config.DNSConfig) {
	m.logger.Info("reloading DNS resolver configuration")
	m.cacheTTL = time.Duration(dnsCfg.CacheTTLSeconds) * time.Second
	m.negativeTTL = time.Duration(dnsCfg.CacheNegativeTTLSeconds) * time.Second
	m.dnsResolver = NewDNSResolver(dnsCfg, m.logger)
}

// CleanupExpiredCache iterates over the map and removes expired entries.
func (m *MXLookup) CleanupExpiredCache() int {
	count := 0
	now := time.Now()
	m.cache.Range(func(key, value any) bool {
		entry, ok := value.(*mxCacheEntry)
		if !ok {
			m.cache.Delete(key)
			count++
			return true
		}
		if now.After(entry.expiresAt) {
			m.cache.Delete(key)
			count++
		}
		return true
	})
	if count > 0 {
		m.logger.Info("cleaned up expired MX cache entries", "count", count)
	}
	return count
}

// singleflightGroup implements rudimentary request coalescing.
type singleflightGroup struct {
	mu sync.Mutex
	m  map[string]*singleflightCall
}

type singleflightCall struct {
	wg  sync.WaitGroup
	val []*MXRecord
	err error
}

func (g *singleflightGroup) do(key string, fn func() ([]*MXRecord, error)) ([]*MXRecord, error) {
	g.mu.Lock()
	if g.m == nil {
		g.m = make(map[string]*singleflightCall)
	}
	if c, ok := g.m[key]; ok {
		g.mu.Unlock()
		c.wg.Wait()
		return c.val, c.err
	}
	c := new(singleflightCall)
	c.wg.Add(1)
	g.m[key] = c
	g.mu.Unlock()

	c.val, c.err = fn()
	c.wg.Done()

	g.mu.Lock()
	delete(g.m, key)
	g.mu.Unlock()

	return c.val, c.err
}
