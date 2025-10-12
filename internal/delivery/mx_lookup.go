package delivery

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"fune/internal/config"
	"fune/internal/queue"
)

// MXLookup handles MX record lookups with two-level caching (successful and negative responses).
// It uses a custom DNS resolver with configurable servers and stores results in SQLite.
// Successful lookups are cached for a longer TTL (default 1 hour), while failed lookups
// use a shorter negative TTL (default 1 minute) to allow faster recovery from transient
// DNS issues.
type MXLookup struct {
	queue       *queue.Queue
	dnsResolver *DNSResolver
	logger      *slog.Logger
	cacheTTL    time.Duration // MX cache TTL (for successful lookups)
	negativeTTL time.Duration // Negative cache TTL (for failures)
}

// MXRecord represents a single MX record with hostname and priority.
// Lower priority values indicate higher preference.
type MXRecord struct {
	Host     string `json:"host"`
	Priority uint16 `json:"priority"`
}

// NewMXLookup creates a new MX lookup service with the specified configuration.
// It initializes a custom DNS resolver and sets cache TTLs from the config.
func NewMXLookup(q *queue.Queue, dnsCfg *config.DNSConfig, deliveryCfg *config.OutboundConfig, logger *slog.Logger) *MXLookup {
	return &MXLookup{
		queue:       q,
		dnsResolver: NewDNSResolver(dnsCfg, logger),
		logger:      logger,
		cacheTTL:    time.Duration(dnsCfg.CacheTTLSeconds) * time.Second,
		negativeTTL: time.Duration(dnsCfg.CacheNegativeTTLSeconds) * time.Second,
	}
}

// Lookup performs MX record lookup for a domain with SQLite-backed caching.
// It first checks the cache, and on cache miss performs a DNS lookup via the custom
// resolver. Results are sorted by priority (lower is higher preference) and stored
// in the cache. Cached results include both successful lookups and negative responses.
func (m *MXLookup) Lookup(ctx context.Context, domain string) ([]*MXRecord, error) {
	// Try cache first
	cached, err := m.getFromCache(domain)
	if err == nil && cached != nil {
		m.logger.Debug("MX cache hit",
			"domain", domain,
			"records", len(cached))
		// Ensure cached records are sorted by priority
		sort.Slice(cached, func(i, j int) bool {
			return cached[i].Priority < cached[j].Priority
		})
		return cached, nil
	}

	// Cache miss - perform DNS lookup
	m.logger.Debug("MX cache miss, performing DNS lookup",
		"domain", domain)

	records, err := m.lookupDNS(ctx, domain)
	if err != nil {
		m.logger.Error("MX lookup failed",
			"domain", domain,
			"error", err)
		return nil, err
	}

	// Sort by priority (lower is higher priority)
	sort.Slice(records, func(i, j int) bool {
		return records[i].Priority < records[j].Priority
	})

	// Store in cache
	if err := m.storeInCache(domain, records); err != nil {
		m.logger.Warn("failed to cache MX records",
			"domain", domain,
			"error", err)
	}

	m.logger.Info("MX lookup successful",
		"domain", domain,
		"records", len(records))

	return records, nil
}

// lookupDNS performs the actual DNS MX lookup using custom resolver
func (m *MXLookup) lookupDNS(ctx context.Context, domain string) ([]*MXRecord, error) {
	// Use custom DNS resolver with timeout
	mxRecords, err := m.dnsResolver.LookupMX(ctx, domain)
	if err != nil {
		// Store negative result in cache
		m.storeNegativeCache(domain)
		return nil, fmt.Errorf("DNS lookup failed: %w", err)
	}

	if len(mxRecords) == 0 {
		m.storeNegativeCache(domain)
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

// storeNegativeCache stores a negative DNS response with shorter TTL
func (m *MXLookup) storeNegativeCache(domain string) {
	// Store empty result with negative TTL
	emptyRecords := []*MXRecord{}
	recordsJSON, _ := json.Marshal(emptyRecords)

	// Use negative TTL (shorter) for failed lookups
	ttlSeconds := int(m.negativeTTL.Seconds())
	if err := m.queue.StoreMXCache(domain, string(recordsJSON), ttlSeconds); err != nil {
		m.logger.Warn("failed to cache negative DNS response",
			"domain", domain,
			"error", err)
	} else {
		m.logger.Debug("cached negative DNS response",
			"domain", domain,
			"ttl", ttlSeconds)
	}
}

// getFromCache retrieves MX records from cache
func (m *MXLookup) getFromCache(domain string) ([]*MXRecord, error) {
	recordsJSON, cachedAt, ttlSeconds, err := m.queue.GetMXCache(domain)
	if err != nil {
		return nil, err // Cache miss
	}

	// Parse cached_at timestamp (SQLite CURRENT_TIMESTAMP uses RFC3339)
	cached, err := time.Parse(time.RFC3339, cachedAt)
	if err != nil {
		return nil, err
	}

	// Check if cache is expired
	if time.Since(cached) > time.Duration(ttlSeconds)*time.Second {
		m.logger.Debug("MX cache expired",
			"domain", domain,
			"age", time.Since(cached))
		return nil, fmt.Errorf("cache expired")
	}

	// Parse JSON records
	var records []*MXRecord
	if err := json.Unmarshal([]byte(recordsJSON), &records); err != nil {
		return nil, err
	}

	return records, nil
}

// storeInCache stores MX records in cache
func (m *MXLookup) storeInCache(domain string, records []*MXRecord) error {
	recordsJSON, err := json.Marshal(records)
	if err != nil {
		return err
	}

	return m.queue.StoreMXCache(domain, string(recordsJSON), int(m.cacheTTL.Seconds()))
}

// InvalidateCache removes a domain's MX records from the cache, forcing a fresh
// DNS lookup on the next Lookup call. This is useful for testing or when DNS
// changes are known to have occurred.
func (m *MXLookup) InvalidateCache(domain string) error {
	err := m.queue.InvalidateMXCache(domain)
	if err != nil {
		m.logger.Error("failed to invalidate cache",
			"domain", domain,
			"error", err)
		return err
	}

	m.logger.Debug("cache invalidated",
		"domain", domain)

	return nil
}

// CleanupExpiredCache removes expired MX cache entries from SQLite.
// Returns the number of entries removed. This should be called periodically
// to prevent unbounded cache growth.
func (m *MXLookup) CleanupExpiredCache() (int, error) {
	rowsAffected, err := m.queue.CleanupExpiredMXCache()
	if err != nil {
		return 0, err
	}

	if rowsAffected > 0 {
		m.logger.Info("cleaned up expired MX cache entries",
			"count", rowsAffected)
	}

	return int(rowsAffected), nil
}

// ReloadConfig updates DNS resolver configuration during a hot reload (triggered by SIGHUP).
// It updates cache TTLs and recreates the DNS resolver with new settings (custom DNS servers,
// timeout). This method enables dynamic DNS configuration changes without restart.
func (m *MXLookup) ReloadConfig(dnsCfg *config.DNSConfig, deliveryCfg *config.OutboundConfig) error {
	m.logger.Info("reloading DNS resolver configuration")

	// Update cache TTLs
	m.cacheTTL = time.Duration(dnsCfg.CacheTTLSeconds) * time.Second
	m.negativeTTL = time.Duration(dnsCfg.CacheNegativeTTLSeconds) * time.Second

	// Recreate DNS resolver with new settings
	m.dnsResolver = NewDNSResolver(dnsCfg, m.logger)

	m.logger.Info("DNS resolver configuration reloaded",
		"cache_ttl", m.cacheTTL,
		"negative_ttl", m.negativeTTL)

	return nil
}

// BatchPrefetch performs parallel MX lookups for multiple domains to warm the cache.
// This is useful when processing a batch of messages to the same domains, as it
// reduces latency by performing DNS lookups concurrently. Already-cached domains
// are skipped. Returns a map of domain to error for any failed lookups.
func (m *MXLookup) BatchPrefetch(ctx context.Context, domains []string) map[string]error {
	if len(domains) == 0 {
		return make(map[string]error)
	}

	// Deduplicate domains and filter out already cached ones
	uniqueDomains := make(map[string]bool)
	domainsToFetch := make([]string, 0, len(domains))

	for _, domain := range domains {
		if uniqueDomains[domain] {
			continue
		}
		uniqueDomains[domain] = true

		// Check if already cached
		cached, err := m.getFromCache(domain)
		if err == nil && cached != nil {
			m.logger.Debug("domain already cached, skipping prefetch",
				"domain", domain)
			continue
		}

		domainsToFetch = append(domainsToFetch, domain)
	}

	if len(domainsToFetch) == 0 {
		m.logger.Debug("all domains already cached, no prefetch needed")
		return make(map[string]error)
	}

	m.logger.Info("batch DNS prefetch starting",
		"total_domains", len(domains),
		"unique_domains", len(uniqueDomains),
		"to_fetch", len(domainsToFetch))

	// Prefetch in parallel
	results := make(map[string]error)
	var mu sync.Mutex
	var wg sync.WaitGroup

	for _, domain := range domainsToFetch {
		wg.Add(1)
		go func(d string) {
			defer wg.Done()

			// Perform lookup (will cache the result)
			_, err := m.Lookup(ctx, d)

			mu.Lock()
			results[d] = err
			mu.Unlock()
		}(domain)
	}

	wg.Wait()

	// Count successes and failures
	successes := 0
	failures := 0
	for _, err := range results {
		if err == nil {
			successes++
		} else {
			failures++
		}
	}

	m.logger.Info("batch DNS prefetch completed",
		"fetched", len(domainsToFetch),
		"successes", successes,
		"failures", failures)

	return results
}
