package delivery

import (
	"context"
	"os"
	"testing"
	"time"

	"fune/internal/config"
	"fune/internal/queue"

	"go.uber.org/zap"
)

func TestBatchPrefetch(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	dbPath := "./test_batch_mx.db"
	defer os.Remove(dbPath)

	q, err := queue.NewQueue(dbPath, logger)
	if err != nil {
		t.Fatalf("Failed to create queue: %v", err)
	}
	defer q.Close()

	deliveryCfg := &config.DeliveryConfig{
		MXCacheTTLSeconds: 3600,
	}

	dnsCfg := &config.DNSConfig{
		CacheTTLSeconds:         3600,
		CacheNegativeTTLSeconds: 60,
		TimeoutSeconds:          5,
	}
	mxLookup := NewMXLookup(q, dnsCfg, deliveryCfg, logger)
	ctx := context.Background()

	// Test with real domains (will actually perform DNS lookups)
	domains := []string{
		"gmail.com",
		"yahoo.com",
		"gmail.com", // Duplicate - should be deduplicated
		"outlook.com",
	}

	results := mxLookup.BatchPrefetch(ctx, domains)

	// Should have 3 results (duplicates removed)
	if len(results) != 3 {
		t.Errorf("Expected 3 unique domains in results, got %d", len(results))
	}

	// Verify domains are now cached
	for _, domain := range []string{"gmail.com", "yahoo.com", "outlook.com"} {
		cached, err := mxLookup.getFromCache(domain)
		if err != nil {
			t.Errorf("Expected %s to be cached, but got error: %v", domain, err)
		}
		if cached == nil {
			t.Errorf("Expected %s to have cached records", domain)
		}
	}
}

func TestBatchPrefetchEmptyList(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	dbPath := "./test_batch_empty.db"
	defer os.Remove(dbPath)

	q, err := queue.NewQueue(dbPath, logger)
	if err != nil {
		t.Fatalf("Failed to create queue: %v", err)
	}
	defer q.Close()

	deliveryCfg := &config.DeliveryConfig{
		MXCacheTTLSeconds: 3600,
	}

	dnsCfg := &config.DNSConfig{
		CacheTTLSeconds:         3600,
		CacheNegativeTTLSeconds: 60,
		TimeoutSeconds:          5,
	}
	mxLookup := NewMXLookup(q, dnsCfg, deliveryCfg, logger)
	ctx := context.Background()

	results := mxLookup.BatchPrefetch(ctx, []string{})

	if len(results) != 0 {
		t.Errorf("Expected 0 results for empty list, got %d", len(results))
	}
}

func TestBatchPrefetchSkipsCached(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	dbPath := "./test_batch_cached.db"
	defer os.Remove(dbPath)

	q, err := queue.NewQueue(dbPath, logger)
	if err != nil {
		t.Fatalf("Failed to create queue: %v", err)
	}
	defer q.Close()

	deliveryCfg := &config.DeliveryConfig{
		MXCacheTTLSeconds: 3600,
	}

	dnsCfg := &config.DNSConfig{
		CacheTTLSeconds:         3600,
		CacheNegativeTTLSeconds: 60,
		TimeoutSeconds:          5,
	}
	mxLookup := NewMXLookup(q, dnsCfg, deliveryCfg, logger)
	ctx := context.Background()

	// Pre-cache one domain
	_, err = mxLookup.Lookup(ctx, "gmail.com")
	if err != nil {
		t.Fatalf("Failed to lookup gmail.com: %v", err)
	}

	// Small delay to ensure cache is written
	time.Sleep(100 * time.Millisecond)

	// Batch prefetch including the already-cached domain
	domains := []string{
		"gmail.com",   // Already cached
		"yahoo.com",   // Not cached
		"outlook.com", // Not cached
	}

	results := mxLookup.BatchPrefetch(ctx, domains)

	// Should only fetch 2 domains (gmail.com already cached)
	if len(results) != 2 {
		t.Errorf("Expected 2 domains to be fetched (skipping cached), got %d", len(results))
	}

	// Verify gmail.com was not in results (was cached)
	if _, found := results["gmail.com"]; found {
		t.Error("gmail.com should not be in results (was already cached)")
	}
}

func TestBatchPrefetchContextCancellation(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	dbPath := "./test_batch_cancel.db"
	defer os.Remove(dbPath)

	q, err := queue.NewQueue(dbPath, logger)
	if err != nil {
		t.Fatalf("Failed to create queue: %v", err)
	}
	defer q.Close()

	deliveryCfg := &config.DeliveryConfig{
		MXCacheTTLSeconds: 3600,
	}

	dnsCfg := &config.DNSConfig{
		CacheTTLSeconds:         3600,
		CacheNegativeTTLSeconds: 60,
		TimeoutSeconds:          5,
	}
	mxLookup := NewMXLookup(q, dnsCfg, deliveryCfg, logger)

	// Create context with immediate cancellation
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately

	domains := []string{"gmail.com", "yahoo.com"}

	// Should handle cancelled context gracefully
	results := mxLookup.BatchPrefetch(ctx, domains)

	// All should fail due to cancelled context
	for domain, err := range results {
		if err == nil {
			t.Errorf("Expected error for %s with cancelled context, got nil", domain)
		}
	}
}
