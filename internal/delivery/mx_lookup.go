package delivery

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"strela/internal/config"
	"strela/internal/recovery"
)

// errNoMXRecords indicates the domain resolved but published no MX records.
// This is a permanent condition (safe to hard-bounce), distinct from a
// transient resolver failure (SERVFAIL, timeout, network error) which must
// stay retryable. Callers in this package compare against it with errors.Is.
var errNoMXRecords = errors.New("no MX records found")

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
//
// err is non-nil only for cached transient failures: it is re-surfaced to the
// caller so that retries within the negative TTL stay retryable, rather than a
// cached nil-records hit being misread as a permanent "no MX records" bounce.
// A permanent no-MX result is cached with records=nil and err=nil.
type mxCacheEntry struct {
	records   []*MXRecord
	err       error
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
// It mitigates cache stampedes using singleflight. The caller's context is
// respected: if it is cancelled while waiting for a shared singleflight lookup,
// Lookup returns immediately with the context error. The underlying DNS query
// uses context.Background() so that one caller's cancellation does not abort
// the lookup for other waiters.
func (m *MXLookup) Lookup(ctx context.Context, domain string) ([]*MXRecord, error) {
	// Fast path: check context before doing any work.
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}

	// Try cache first. A cached transient failure carries its error so it stays
	// retryable; a cached permanent no-MX result has nil records and nil error.
	if cached, cachedErr, ok := m.getFromCache(domain); ok {
		if cachedErr != nil {
			m.logger.Debug("MX negative cache hit (transient failure)", "domain", domain, "error", cachedErr)
			return nil, cachedErr
		}
		m.logger.Debug("MX cache hit", "domain", domain, "records", len(cached))
		return cached, nil
	}

	// Cache miss - join singleflight.
	// The singleflight's do() blocks until the shared lookup finishes.
	// We run it in a goroutine so we can also select on ctx.Done(), allowing
	// the caller to bail out early without abandoning the shared lookup.
	m.logger.Debug("MX cache miss, performing DNS lookup", "domain", domain)

	type sfResult struct {
		records []*MXRecord
		err     error
	}
	ch := make(chan sfResult, 1)
	recovery.SafeGo(m.logger, "mx-singleflight-lookup", func() {
		result, err := m.sf.do(domain, func() ([]*MXRecord, error) {
			m.logger.Debug("executing singleflight DNS lookup", "domain", domain)

			// Use context.Background() to ensure one client cancelling doesn't
			// abort the lookup for others. The resolver enforces its own timeout.
			records, err := m.lookupDNS(context.Background(), domain)
			if err != nil {
				m.logger.Debug("MX lookup failed", "domain", domain, "error", err)
				if errors.Is(err, errNoMXRecords) {
					// Permanent: domain resolves but publishes no MX records.
					// Cache an empty, non-error result so a cached hit is treated
					// as a hard bounce, consistent with a fresh lookup.
					m.storeInCache(domain, nil, nil, m.negativeTTL)
				} else {
					// Transient resolver failure (SERVFAIL, timeout, network).
					// Cache the error so retries within the negative TTL remain
					// retryable instead of being misclassified as a permanent
					// "no MX records" hard bounce.
					m.storeInCache(domain, nil, err, m.negativeTTL)
				}
				return nil, err
			}

			// Sort by priority (lower is higher priority)
			sort.Slice(records, func(i, j int) bool {
				return records[i].Priority < records[j].Priority
			})

			// Store in cache
			m.storeInCache(domain, records, nil, m.cacheTTL)

			m.logger.Info("MX lookup successful", "domain", domain, "records", len(records))
			return records, nil
		})
		ch <- sfResult{records: result, err: err}
	})

	select {
	case r := <-ch:
		return r.records, r.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// lookupDNS performs the actual DNS MX lookup.
func (m *MXLookup) lookupDNS(ctx context.Context, domain string) ([]*MXRecord, error) {
	mxRecords, err := m.dnsResolver.LookupMX(ctx, domain)
	if err != nil {
		return nil, fmt.Errorf("DNS lookup failed: %w", err)
	}

	if len(mxRecords) == 0 {
		return nil, fmt.Errorf("%w for domain %s", errNoMXRecords, domain)
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

// getFromCache retrieves and validates MX records from cache. The returned
// error is the cached transient-failure error (nil for hits and permanent
// no-MX results); ok reports whether a live cache entry was found.
func (m *MXLookup) getFromCache(domain string) ([]*MXRecord, error, bool) {
	val, ok := m.cache.Load(domain)
	if !ok {
		return nil, nil, false
	}

	entry, ok := val.(*mxCacheEntry)
	if !ok {
		m.logger.Error("type assertion failed for MX cache entry",
			"domain", domain,
			"type", fmt.Sprintf("%T", val))
		m.cache.Delete(domain) // Remove corrupted entry
		return nil, nil, false
	}

	if time.Now().After(entry.expiresAt) {
		m.cache.Delete(domain)
		return nil, nil, false
	}

	return entry.records, entry.err, true
}

// storeInCache stores an MX lookup result in cache. A non-nil err marks a
// cached transient failure that is re-surfaced (kept retryable) on later hits.
func (m *MXLookup) storeInCache(domain string, records []*MXRecord, err error, ttl time.Duration) {
	m.cache.Store(domain, &mxCacheEntry{
		records:   records,
		err:       err,
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
