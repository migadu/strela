package delivery

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"fune/internal/config"
	"fune/internal/queue"

	"go.uber.org/zap"
)

func TestMXLookup_LookupDNS(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	q, cleanup := queue.SetupTestQueue(t)
	defer cleanup()

	dnsCfg := &config.DNSConfig{
		TimeoutSeconds:          5,
		CacheTTLSeconds:         3600,
		CacheNegativeTTLSeconds: 60,
	}
	deliveryCfg := &config.OutboundConfig{
		MXCacheTTLSeconds: 3600,
	}

	mx := NewMXLookup(q, dnsCfg, deliveryCfg, logger)

	// Test lookup for a well-known domain
	ctx := context.Background()
	records, err := mx.lookupDNS(ctx, "gmail.com")
	if err != nil {
		t.Fatalf("Failed to lookup MX for gmail.com: %v", err)
	}

	if len(records) == 0 {
		t.Error("Expected at least one MX record for gmail.com")
	}

	// Verify records have required fields
	for _, record := range records {
		if record.Host == "" {
			t.Error("MX record missing host")
		}
		if record.Priority == 0 {
			t.Error("MX record has zero priority (unexpected)")
		}
	}

	// Verify sorting by priority
	for i := 1; i < len(records); i++ {
		if records[i-1].Priority > records[i].Priority {
			t.Error("MX records not sorted by priority")
			break
		}
	}
}

func TestMXLookup_LookupDNS_NoMXRecords(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	q, cleanup := queue.SetupTestQueue(t)
	defer cleanup()

	dnsCfg := &config.DNSConfig{
		TimeoutSeconds:          5,
		CacheTTLSeconds:         3600,
		CacheNegativeTTLSeconds: 60,
	}
	deliveryCfg := &config.OutboundConfig{
		MXCacheTTLSeconds: 3600,
	}

	mx := NewMXLookup(q, dnsCfg, deliveryCfg, logger)

	// Test domain with no MX records (should fail)
	ctx := context.Background()
	_, err := mx.lookupDNS(ctx, "localhost")
	if err == nil {
		t.Error("Expected error for domain with no MX records")
	}
}

func TestMXLookup_LookupDNS_InvalidDomain(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	q, cleanup := queue.SetupTestQueue(t)
	defer cleanup()

	dnsCfg := &config.DNSConfig{
		TimeoutSeconds:          5,
		CacheTTLSeconds:         3600,
		CacheNegativeTTLSeconds: 60,
	}
	deliveryCfg := &config.OutboundConfig{
		MXCacheTTLSeconds: 3600,
	}

	mx := NewMXLookup(q, dnsCfg, deliveryCfg, logger)

	// Test invalid domain
	ctx := context.Background()
	_, err := mx.lookupDNS(ctx, "invalid-domain-that-does-not-exist-12345.com")
	if err == nil {
		t.Error("Expected error for invalid domain")
	}
}

func TestMXLookup_Cache(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	q, cleanup := queue.SetupTestQueue(t)
	defer cleanup()

	dnsCfg := &config.DNSConfig{
		TimeoutSeconds:          5,
		CacheTTLSeconds:         3600,
		CacheNegativeTTLSeconds: 60,
	}
	deliveryCfg := &config.OutboundConfig{
		MXCacheTTLSeconds: 3600,
	}

	mx := NewMXLookup(q, dnsCfg, deliveryCfg, logger)

	// First lookup - should hit DNS
	records1, err := mx.Lookup(context.Background(), "gmail.com")
	if err != nil {
		t.Fatalf("First lookup failed: %v", err)
	}

	// Second lookup - should hit cache
	records2, err := mx.Lookup(context.Background(), "gmail.com")
	if err != nil {
		t.Fatalf("Second lookup failed: %v", err)
	}

	// Verify same number of records
	if len(records1) != len(records2) {
		t.Errorf("Cache returned different number of records: %d vs %d", len(records1), len(records2))
	}

	// Verify records match
	for i := range records1 {
		if records1[i].Host != records2[i].Host {
			t.Errorf("Cached record host mismatch: %s vs %s", records1[i].Host, records2[i].Host)
		}
		if records1[i].Priority != records2[i].Priority {
			t.Errorf("Cached record priority mismatch: %d vs %d", records1[i].Priority, records2[i].Priority)
		}
	}
}

func TestMXLookup_CacheExpiration(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	q, cleanup := queue.SetupTestQueue(t)
	defer cleanup()

	// Create MX lookup with 1 second TTL
	dnsCfg := &config.DNSConfig{
		TimeoutSeconds:          5,
		CacheTTLSeconds:         1,
		CacheNegativeTTLSeconds: 60,
	}
	deliveryCfg := &config.OutboundConfig{
		MXCacheTTLSeconds: 1,
	}
	mx := NewMXLookup(q, dnsCfg, deliveryCfg, logger)

	// First lookup
	_, err := mx.Lookup(context.Background(), "gmail.com")
	if err != nil {
		t.Fatalf("First lookup failed: %v", err)
	}

	// Wait for cache to expire
	time.Sleep(2 * time.Second)

	// Try to get from cache - should fail due to expiration
	cached, err := mx.getFromCache("gmail.com")
	if err == nil {
		t.Error("Expected cache miss for expired entry")
	}
	if cached != nil {
		t.Error("Expected nil cached records for expired entry")
	}
}

func TestMXLookup_StoreAndRetrieveCache(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	q, cleanup := queue.SetupTestQueue(t)
	defer cleanup()

	dnsCfg := &config.DNSConfig{
		TimeoutSeconds:          5,
		CacheTTLSeconds:         3600,
		CacheNegativeTTLSeconds: 60,
	}
	deliveryCfg := &config.OutboundConfig{
		MXCacheTTLSeconds: 3600,
	}

	mx := NewMXLookup(q, dnsCfg, deliveryCfg, logger)

	// Create test records
	testRecords := []*MXRecord{
		{Host: "mx1.example.com", Priority: 10},
		{Host: "mx2.example.com", Priority: 20},
	}

	// Store in cache
	err := mx.storeInCache("example.com", testRecords)
	if err != nil {
		t.Fatalf("Failed to store in cache: %v", err)
	}

	// Retrieve from cache
	cached, err := mx.getFromCache("example.com")
	if err != nil {
		t.Fatalf("Failed to get from cache: %v", err)
	}

	if len(cached) != len(testRecords) {
		t.Errorf("Expected %d cached records, got %d", len(testRecords), len(cached))
	}

	for i := range testRecords {
		if cached[i].Host != testRecords[i].Host {
			t.Errorf("Host mismatch: %s vs %s", cached[i].Host, testRecords[i].Host)
		}
		if cached[i].Priority != testRecords[i].Priority {
			t.Errorf("Priority mismatch: %d vs %d", cached[i].Priority, testRecords[i].Priority)
		}
	}
}

func TestMXLookup_InvalidateCache(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	q, cleanup := queue.SetupTestQueue(t)
	defer cleanup()

	dnsCfg := &config.DNSConfig{
		TimeoutSeconds:          5,
		CacheTTLSeconds:         3600,
		CacheNegativeTTLSeconds: 60,
	}
	deliveryCfg := &config.OutboundConfig{
		MXCacheTTLSeconds: 3600,
	}

	mx := NewMXLookup(q, dnsCfg, deliveryCfg, logger)

	// Store test record
	testRecords := []*MXRecord{
		{Host: "mx1.example.com", Priority: 10},
	}
	mx.storeInCache("example.com", testRecords)

	// Verify it's cached
	cached, err := mx.getFromCache("example.com")
	if err != nil || cached == nil {
		t.Fatal("Record should be in cache")
	}

	// Invalidate
	err = mx.InvalidateCache("example.com")
	if err != nil {
		t.Fatalf("Failed to invalidate cache: %v", err)
	}

	// Verify it's gone
	cached, err = mx.getFromCache("example.com")
	if err == nil && cached != nil {
		t.Error("Cache should be invalidated")
	}
}

func TestMXLookup_CleanupExpiredCache(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	q, cleanup := queue.SetupTestQueue(t)
	defer cleanup()

	dnsCfg := &config.DNSConfig{
		TimeoutSeconds:          5,
		CacheTTLSeconds:         1,
		CacheNegativeTTLSeconds: 60,
	}
	deliveryCfg := &config.OutboundConfig{
		MXCacheTTLSeconds: 1,
	}
	mx := NewMXLookup(q, dnsCfg, deliveryCfg, logger)

	// Store multiple records
	testRecords := []*MXRecord{
		{Host: "mx1.example.com", Priority: 10},
	}

	mx.storeInCache("domain1.com", testRecords)
	mx.storeInCache("domain2.com", testRecords)

	// Wait for expiration
	time.Sleep(2 * time.Second)

	// Cleanup expired entries
	count, err := mx.CleanupExpiredCache()
	if err != nil {
		t.Fatalf("Failed to cleanup cache: %v", err)
	}

	if count != 2 {
		t.Errorf("Expected 2 expired entries cleaned, got %d", count)
	}

	// Verify entries are gone
	cached, _ := mx.getFromCache("domain1.com")
	if cached != nil {
		t.Error("domain1.com should be cleaned up")
	}

	cached, _ = mx.getFromCache("domain2.com")
	if cached != nil {
		t.Error("domain2.com should be cleaned up")
	}
}

func TestMXLookup_SortByPriority(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	q, cleanup := queue.SetupTestQueue(t)
	defer cleanup()

	dnsCfg := &config.DNSConfig{
		TimeoutSeconds:          5,
		CacheTTLSeconds:         3600,
		CacheNegativeTTLSeconds: 60,
	}
	deliveryCfg := &config.OutboundConfig{
		MXCacheTTLSeconds: 3600,
	}

	mx := NewMXLookup(q, dnsCfg, deliveryCfg, logger)

	// Create unsorted records
	unsorted := []*MXRecord{
		{Host: "mx3.example.com", Priority: 30},
		{Host: "mx1.example.com", Priority: 10},
		{Host: "mx2.example.com", Priority: 20},
	}

	// Store and retrieve (lookup does sorting)
	mx.storeInCache("example.com", unsorted)

	// Perform lookup which should sort
	records, err := mx.Lookup(context.Background(), "example.com")
	if err != nil {
		t.Fatalf("Lookup failed: %v", err)
	}

	// Verify sorted by priority
	expectedPriorities := []uint16{10, 20, 30}
	for i, record := range records {
		if record.Priority != expectedPriorities[i] {
			t.Errorf("Record %d: expected priority %d, got %d", i, expectedPriorities[i], record.Priority)
		}
	}
}

func TestMXRecord_JSONSerialization(t *testing.T) {
	records := []*MXRecord{
		{Host: "mx1.example.com", Priority: 10},
		{Host: "mx2.example.com", Priority: 20},
	}

	// Marshal to JSON
	jsonData, err := json.Marshal(records)
	if err != nil {
		t.Fatalf("Failed to marshal JSON: %v", err)
	}

	// Unmarshal from JSON
	var decoded []*MXRecord
	err = json.Unmarshal(jsonData, &decoded)
	if err != nil {
		t.Fatalf("Failed to unmarshal JSON: %v", err)
	}

	// Verify
	if len(decoded) != len(records) {
		t.Errorf("Expected %d records, got %d", len(records), len(decoded))
	}

	for i := range records {
		if decoded[i].Host != records[i].Host {
			t.Errorf("Host mismatch: %s vs %s", decoded[i].Host, records[i].Host)
		}
		if decoded[i].Priority != records[i].Priority {
			t.Errorf("Priority mismatch: %d vs %d", decoded[i].Priority, records[i].Priority)
		}
	}
}
