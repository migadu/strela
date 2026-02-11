package srs

import (
	"fmt"
	"strings"
	"testing"
)

func TestNewSRS_Success(t *testing.T) {
	srs, err := NewSRS([]string{"example.com"}, "round-robin", "my-secret-key-123", 21, 4, "=", false)
	if err != nil {
		t.Fatalf("NewSRS failed: %v", err)
	}
	if len(srs.domains) != 1 || srs.domains[0] != "example.com" {
		t.Errorf("Expected domains ['example.com'], got %v", srs.domains)
	}
	if srs.maxAge != 21 {
		t.Errorf("Expected maxAge 21, got %d", srs.maxAge)
	}
	if srs.hashLength != 4 {
		t.Errorf("Expected hashLength 4, got %d", srs.hashLength)
	}
}

func TestNewSRS_EmptyDomain(t *testing.T) {
	_, err := NewSRS([]string{}, "round-robin", "my-secret-key-123", 21, 4, "=", false)
	if err == nil {
		t.Error("Expected error for empty domain list, got nil")
	}
	if !strings.Contains(err.Error(), "domains list cannot be empty") {
		t.Errorf("Expected 'domains list cannot be empty' error, got: %v", err)
	}
}

func TestNewSRS_EmptySecret(t *testing.T) {
	_, err := NewSRS([]string{"example.com"}, "round-robin", "", 21, 4, "=", false)
	if err == nil {
		t.Error("Expected error for empty secret, got nil")
	}
}

func TestNewSRS_ShortSecret(t *testing.T) {
	_, err := NewSRS([]string{"example.com"}, "round-robin", "short", 21, 4, "=", false)
	if err == nil {
		t.Error("Expected error for short secret, got nil")
	}
	if !strings.Contains(err.Error(), "at least 16 characters") {
		t.Errorf("Expected 'at least 16 characters' error, got: %v", err)
	}
}

func TestNewSRS_InvalidHashLength(t *testing.T) {
	tests := []int{0, 1, 9, 10}
	for _, hashLen := range tests {
		_, err := NewSRS([]string{"example.com"}, "round-robin", "my-secret-key-123", 21, hashLen, "=", false)
		if err == nil {
			t.Errorf("Expected error for hash length %d, got nil", hashLen)
		}
	}
}

func TestForward_Basic(t *testing.T) {
	srs, err := NewSRS([]string{"forwarding.com"}, "round-robin", "my-secret-key-123456", 21, 4, "=", false)
	if err != nil {
		t.Fatalf("NewSRS failed: %v", err)
	}

	original := "user@example.com"
	rewritten, err := srs.Forward(original)
	if err != nil {
		t.Fatalf("Forward failed: %v", err)
	}

	// Check format
	if !strings.HasPrefix(rewritten, "SRS0=") {
		t.Errorf("Expected SRS0 prefix, got: %s", rewritten)
	}
	if !strings.HasSuffix(rewritten, "@forwarding.com") {
		t.Errorf("Expected @forwarding.com suffix, got: %s", rewritten)
	}

	// Check that original components are present
	if !strings.Contains(rewritten, "example.com") {
		t.Errorf("Rewritten address missing original domain: %s", rewritten)
	}
	if !strings.Contains(rewritten, "user") {
		t.Errorf("Rewritten address missing original localpart: %s", rewritten)
	}
}

func TestForward_EmptyAddress(t *testing.T) {
	srs, _ := NewSRS([]string{"forwarding.com"}, "round-robin", "my-secret-key-123456", 21, 4, "=", false)

	_, err := srs.Forward("")
	if err == nil {
		t.Error("Expected error for empty address, got nil")
	}
}

func TestForward_InvalidAddress(t *testing.T) {
	srs, _ := NewSRS([]string{"forwarding.com"}, "round-robin", "my-secret-key-123456", 21, 4, "=", false)

	invalidAddresses := []string{
		"invalid",
		"@example.com",
		"user@",
		"user@@example.com",
	}

	for _, addr := range invalidAddresses {
		_, err := srs.Forward(addr)
		if err == nil {
			t.Errorf("Expected error for invalid address '%s', got nil", addr)
		}
	}
}

func TestForwardReverse_RoundTrip(t *testing.T) {
	srs, err := NewSRS([]string{"forwarding.com"}, "round-robin", "my-secret-key-123456", 21, 4, "=", false)
	if err != nil {
		t.Fatalf("NewSRS failed: %v", err)
	}

	original := "user@example.com"

	// Forward
	rewritten, err := srs.Forward(original)
	if err != nil {
		t.Fatalf("Forward failed: %v", err)
	}

	// Reverse
	reversed, err := srs.Reverse(rewritten)
	if err != nil {
		t.Fatalf("Reverse failed: %v", err)
	}

	if reversed != original {
		t.Errorf("Round-trip failed: expected '%s', got '%s'", original, reversed)
	}
}

func TestForwardReverse_WithSpecialCharacters(t *testing.T) {
	srs, _ := NewSRS([]string{"forwarding.com"}, "round-robin", "my-secret-key-123456", 21, 4, "=", false)

	testCases := []string{
		"user+tag@example.com",
		"user.name@example.com",
		"user_name@example.com",
		"123@example.com",
	}

	for _, original := range testCases {
		rewritten, err := srs.Forward(original)
		if err != nil {
			t.Errorf("Forward failed for '%s': %v", original, err)
			continue
		}

		reversed, err := srs.Reverse(rewritten)
		if err != nil {
			t.Errorf("Reverse failed for '%s': %v", original, err)
			continue
		}

		if reversed != original {
			t.Errorf("Round-trip failed for '%s': got '%s'", original, reversed)
		}
	}
}

func TestForward_SRS0ToSRS1(t *testing.T) {
	srs, _ := NewSRS([]string{"forwarding2.com"}, "round-robin", "my-secret-key-123456", 21, 4, "=", false)

	// Create an SRS0 address
	srs0 := "SRS0=ABCD=TT=example.com=user@forwarding1.com"

	// Forward it again (should become SRS1)
	srs1, err := srs.Forward(srs0)
	if err != nil {
		t.Fatalf("Forward SRS0 to SRS1 failed: %v", err)
	}

	if !strings.HasPrefix(srs1, "SRS1=") {
		t.Errorf("Expected SRS1 prefix, got: %s", srs1)
	}
	if !strings.HasSuffix(srs1, "@forwarding2.com") {
		t.Errorf("Expected @forwarding2.com suffix, got: %s", srs1)
	}
}

func TestForward_SRS1StaysSRS1(t *testing.T) {
	srs, _ := NewSRS([]string{"forwarding2.com"}, "round-robin", "my-secret-key-123456", 21, 4, "=", false)

	// Create an SRS1 address
	srs1Original := "SRS1=ABCD=forwarding1.com=SRS0=WXYZ=TT=example.com=user@forwarding1.com"

	// Forward it again (should stay SRS1)
	srs1Result, err := srs.Forward(srs1Original)
	if err != nil {
		t.Fatalf("Forward SRS1 failed: %v", err)
	}

	// Should return the same address
	if srs1Result != srs1Original {
		t.Errorf("Expected SRS1 to stay unchanged: got '%s'", srs1Result)
	}
}

func TestIsSRS(t *testing.T) {
	srs, _ := NewSRS([]string{"forwarding.com"}, "round-robin", "my-secret-key-123456", 21, 4, "=", false)

	tests := []struct {
		address  string
		expected bool
	}{
		{"user@example.com", false},
		{"SRS0=ABCD=TT=example.com=user@forwarding.com", true},
		{"SRS1=ABCD=example.com=SRS0=WXYZ=TT=test.com=user@forwarding.com", true},
		{"srs0=abcd=tt=example.com=user@forwarding.com", true}, // Case insensitive
		{"NOTANRSS@example.com", false},
	}

	for _, tt := range tests {
		result := srs.IsSRS(tt.address)
		if result != tt.expected {
			t.Errorf("IsSRS(%s): expected %v, got %v", tt.address, tt.expected, result)
		}
	}
}

func TestReverse_NotSRS(t *testing.T) {
	srs, _ := NewSRS([]string{"forwarding.com"}, "round-robin", "my-secret-key-123456", 21, 4, "=", false)

	_, err := srs.Reverse("user@example.com")
	if err == nil {
		t.Error("Expected error for non-SRS address, got nil")
	}
	if !strings.Contains(err.Error(), "not an SRS address") {
		t.Errorf("Expected 'not an SRS address' error, got: %v", err)
	}
}

func TestReverse_InvalidHash(t *testing.T) {
	srs, _ := NewSRS([]string{"forwarding.com"}, "round-robin", "my-secret-key-123456", 21, 4, "=", false)

	// Create an SRS address with wrong hash but valid timestamp
	currentTimestamp := srs.generateTimestamp()
	invalidSRS := fmt.Sprintf("SRS0=XXXX=%s=example.com=user@forwarding.com", currentTimestamp)

	_, err := srs.Reverse(invalidSRS)
	if err == nil {
		t.Error("Expected error for invalid hash, got nil")
	}
	if !strings.Contains(err.Error(), "hash validation failed") && !strings.Contains(err.Error(), "signature mismatch") {
		t.Errorf("Expected 'hash validation failed' or 'signature mismatch' error, got: %v", err)
	}
}

func TestReverse_InvalidFormat(t *testing.T) {
	srs, _ := NewSRS([]string{"forwarding.com"}, "round-robin", "my-secret-key-123456", 21, 4, "=", false)

	invalidFormats := []string{
		"SRS0=ABCD@forwarding.com",                // Missing parts
		"SRS0=ABCD=TT@forwarding.com",             // Missing parts
		"SRS0=ABCD=TT=example.com@forwarding.com", // Missing localpart
	}

	for _, addr := range invalidFormats {
		_, err := srs.Reverse(addr)
		if err == nil {
			t.Errorf("Expected error for invalid format '%s', got nil", addr)
		}
	}
}

func TestParseEmail(t *testing.T) {
	tests := []struct {
		input        string
		expectLocal  string
		expectDomain string
		expectError  bool
	}{
		{"user@example.com", "user", "example.com", false},
		{"User@Example.Com", "User", "example.com", false},   // localpart case preserved, domain lowercased
		{"<user@example.com>", "user", "example.com", false}, // Angle brackets
		{"user+tag@example.com", "user+tag", "example.com", false},
		{"invalid", "", "", true},
		{"@example.com", "", "", true},
		{"user@", "", "", true},
	}

	for _, tt := range tests {
		local, domain, err := parseEmail(tt.input)
		if tt.expectError {
			if err == nil {
				t.Errorf("parseEmail(%s): expected error, got nil", tt.input)
			}
		} else {
			if err != nil {
				t.Errorf("parseEmail(%s): unexpected error: %v", tt.input, err)
			}
			if local != tt.expectLocal {
				t.Errorf("parseEmail(%s): expected local '%s', got '%s'", tt.input, tt.expectLocal, local)
			}
			if domain != tt.expectDomain {
				t.Errorf("parseEmail(%s): expected domain '%s', got '%s'", tt.input, tt.expectDomain, domain)
			}
		}
	}
}

func TestGenerateHash_Consistency(t *testing.T) {
	srs, _ := NewSRS([]string{"forwarding.com"}, "round-robin", "my-secret-key-123456", 21, 4, "=", false)

	// Generate hash multiple times with same input
	hash1 := srs.generateHash("timestamp", "example.com", "user")
	hash2 := srs.generateHash("timestamp", "example.com", "user")
	hash3 := srs.generateHash("timestamp", "example.com", "user")

	if hash1 != hash2 || hash2 != hash3 {
		t.Errorf("Hash not consistent: %s, %s, %s", hash1, hash2, hash3)
	}

	// Different input should produce different hash
	hash4 := srs.generateHash("timestamp", "example.com", "different")
	if hash1 == hash4 {
		t.Error("Different inputs produced same hash")
	}
}

func TestGenerateHash_Length(t *testing.T) {
	tests := []int{2, 4, 6, 8}

	for _, hashLen := range tests {
		srs, _ := NewSRS([]string{"forwarding.com"}, "round-robin", "my-secret-key-123456", 21, hashLen, "=", false)
		hash := srs.generateHash("test", "example.com", "user")

		if len(hash) != hashLen {
			t.Errorf("Expected hash length %d, got %d (hash: %s)", hashLen, len(hash), hash)
		}
	}
}

func TestGenerateTimestamp(t *testing.T) {
	srs, _ := NewSRS([]string{"forwarding.com"}, "round-robin", "my-secret-key-123456", 21, 4, "=", false)

	timestamp1 := srs.generateTimestamp()
	timestamp2 := srs.generateTimestamp()

	// Timestamps generated within same second should be identical
	if timestamp1 != timestamp2 {
		t.Errorf("Timestamps differ within same second: %s vs %s", timestamp1, timestamp2)
	}

	// Timestamp should be 2 characters
	if len(timestamp1) != 2 {
		t.Errorf("Expected timestamp length 2, got %d", len(timestamp1))
	}
}

func TestValidateTimestamp_Current(t *testing.T) {
	srs, _ := NewSRS([]string{"forwarding.com"}, "round-robin", "my-secret-key-123456", 21, 4, "=", false)

	// Generate current timestamp
	timestamp := srs.generateTimestamp()

	// Should validate successfully
	err := srs.validateTimestamp(timestamp)
	if err != nil {
		t.Errorf("Current timestamp validation failed: %v", err)
	}
}

func TestGetDomain(t *testing.T) {
	srs, _ := NewSRS([]string{"forwarding.com"}, "round-robin", "my-secret-key-123456", 21, 4, "=", false)

	if srs.GetDomain() != "forwarding.com" {
		t.Errorf("Expected domain 'forwarding.com', got '%s'", srs.GetDomain())
	}
}

func TestDifferentSecrets_DifferentHashes(t *testing.T) {
	srs1, _ := NewSRS([]string{"forwarding.com"}, "round-robin", "secret-key-number-1", 21, 4, "=", false)
	srs2, _ := NewSRS([]string{"forwarding.com"}, "round-robin", "secret-key-number-2", 21, 4, "=", false)

	original := "user@example.com"

	rewritten1, _ := srs1.Forward(original)
	rewritten2, _ := srs2.Forward(original)

	// Different secrets should produce different SRS addresses
	if rewritten1 == rewritten2 {
		t.Error("Different secrets produced identical SRS addresses")
	}

	// Each should reverse with its own secret
	reversed1, err1 := srs1.Reverse(rewritten1)
	reversed2, err2 := srs2.Reverse(rewritten2)

	if err1 != nil || err2 != nil {
		t.Errorf("Reverse with matching secret failed: %v, %v", err1, err2)
	}
	if reversed1 != original || reversed2 != original {
		t.Error("Reverse with matching secret didn't return original")
	}

	// Cross-validation should fail
	_, err := srs1.Reverse(rewritten2)
	if err == nil {
		t.Error("Expected error when reversing with wrong secret, got nil")
	}
}

func TestCustomSeparator(t *testing.T) {
	srs, _ := NewSRS([]string{"forwarding.com"}, "round-robin", "my-secret-key-123456", 21, 4, "+", false)

	original := "user@example.com"
	rewritten, err := srs.Forward(original)
	if err != nil {
		t.Fatalf("Forward failed: %v", err)
	}

	// Check that custom separator is used
	if !strings.Contains(rewritten, "+") {
		t.Errorf("Expected custom separator '+' in address: %s", rewritten)
	}
	if strings.Contains(rewritten, "=") {
		t.Errorf("Should not contain default separator '=' in address: %s", rewritten)
	}

	// Reverse should work
	reversed, err := srs.Reverse(rewritten)
	if err != nil {
		t.Fatalf("Reverse failed: %v", err)
	}
	if reversed != original {
		t.Errorf("Expected '%s', got '%s'", original, reversed)
	}
}

// Multi-domain SRS tests

func TestMultiDomain_RoundRobin(t *testing.T) {
	domains := []string{"srs1.example.com", "srs2.example.com", "srs3.example.com"}
	srs, err := NewSRS(domains, "round-robin", "my-secret-key-123456", 21, 4, "=", false)
	if err != nil {
		t.Fatalf("NewSRS failed: %v", err)
	}

	// Forward 10 messages and collect domains used
	domainCounts := make(map[string]int)
	for i := 0; i < 10; i++ {
		sender := fmt.Sprintf("user%d@original.com", i)
		rewritten, err := srs.Forward(sender)
		if err != nil {
			t.Fatalf("Forward failed: %v", err)
		}

		// Extract domain from SRS address
		parts := strings.Split(rewritten, "@")
		if len(parts) != 2 {
			t.Fatalf("Invalid SRS address format: %s", rewritten)
		}
		domain := parts[1]
		domainCounts[domain]++
	}

	// Check that all domains were used
	for _, domain := range domains {
		if domainCounts[domain] == 0 {
			t.Errorf("Domain %s was never used in round-robin", domain)
		}
	}

	// Check distribution is roughly even (each should be used 3-4 times out of 10)
	for domain, count := range domainCounts {
		if count < 2 || count > 5 {
			t.Errorf("Domain %s used %d times, expected 2-5 for round-robin distribution", domain, count)
		}
	}
}

func TestMultiDomain_HashSender(t *testing.T) {
	domains := []string{"srs1.example.com", "srs2.example.com", "srs3.example.com"}
	srs, err := NewSRS(domains, "hash-sender", "my-secret-key-123456", 21, 4, "=", false)
	if err != nil {
		t.Fatalf("NewSRS failed: %v", err)
	}

	// Forward same sender multiple times - should always use same domain
	sender := "user@original.com"
	var firstDomain string

	for i := 0; i < 5; i++ {
		rewritten, err := srs.Forward(sender)
		if err != nil {
			t.Fatalf("Forward failed: %v", err)
		}

		parts := strings.Split(rewritten, "@")
		if len(parts) != 2 {
			t.Fatalf("Invalid SRS address format: %s", rewritten)
		}
		domain := parts[1]

		if i == 0 {
			firstDomain = domain
		} else if domain != firstDomain {
			t.Errorf("hash-sender strategy inconsistent: first=%s, iteration %d=%s", firstDomain, i, domain)
		}
	}

	// Forward different senders - should distribute across domains
	domainCounts := make(map[string]int)
	for i := 0; i < 20; i++ {
		sender := fmt.Sprintf("user%d@original.com", i)
		rewritten, err := srs.Forward(sender)
		if err != nil {
			t.Fatalf("Forward failed: %v", err)
		}

		parts := strings.Split(rewritten, "@")
		domain := parts[1]
		domainCounts[domain]++
	}

	// All domains should be used at least once
	for _, domain := range domains {
		if domainCounts[domain] == 0 {
			t.Errorf("Domain %s was never used in hash-sender", domain)
		}
	}
}

func TestMultiDomain_ReverseAnyDomain(t *testing.T) {
	domains := []string{"srs1.example.com", "srs2.example.com", "srs3.example.com"}
	srs, err := NewSRS(domains, "round-robin", "my-secret-key-123456", 21, 4, "=", false)
	if err != nil {
		t.Fatalf("NewSRS failed: %v", err)
	}

	original := "user@original.com"

	// Forward multiple times to get different SRS domains
	for i := 0; i < 10; i++ {
		rewritten, err := srs.Forward(original)
		if err != nil {
			t.Fatalf("Forward failed: %v", err)
		}

		// Reverse should work regardless of which domain was used
		reversed, err := srs.Reverse(rewritten)
		if err != nil {
			t.Fatalf("Reverse failed for %s: %v", rewritten, err)
		}

		if reversed != original {
			t.Errorf("Reverse failed: expected %s, got %s", original, reversed)
		}
	}
}

func TestMultiDomain_InvalidSelection(t *testing.T) {
	domains := []string{"srs1.example.com", "srs2.example.com"}
	_, err := NewSRS(domains, "invalid-strategy", "my-secret-key-123456", 21, 4, "=", false)
	if err == nil {
		t.Error("Expected error for invalid selection strategy, got nil")
	}
	if !strings.Contains(err.Error(), "round-robin") || !strings.Contains(err.Error(), "hash-sender") {
		t.Errorf("Error should mention valid strategies, got: %v", err)
	}
}

func TestMultiDomain_SingleDomain(t *testing.T) {
	// Single domain should work with both strategies
	domains := []string{"srs1.example.com"}

	for _, strategy := range []string{"round-robin", "hash-sender"} {
		srs, err := NewSRS(domains, strategy, "my-secret-key-123456", 21, 4, "=", false)
		if err != nil {
			t.Fatalf("NewSRS failed with strategy %s: %v", strategy, err)
		}

		original := "user@original.com"
		rewritten, err := srs.Forward(original)
		if err != nil {
			t.Fatalf("Forward failed: %v", err)
		}

		if !strings.HasSuffix(rewritten, "@srs1.example.com") {
			t.Errorf("Expected single domain to be used, got: %s", rewritten)
		}

		reversed, err := srs.Reverse(rewritten)
		if err != nil {
			t.Fatalf("Reverse failed: %v", err)
		}
		if reversed != original {
			t.Errorf("Round-trip failed: expected %s, got %s", original, reversed)
		}
	}
}

func TestGetDomains(t *testing.T) {
	domains := []string{"srs1.example.com", "srs2.example.com", "srs3.example.com"}
	srs, err := NewSRS(domains, "round-robin", "my-secret-key-123456", 21, 4, "=", false)
	if err != nil {
		t.Fatalf("NewSRS failed: %v", err)
	}

	returnedDomains := srs.GetDomains()
	if len(returnedDomains) != len(domains) {
		t.Errorf("Expected %d domains, got %d", len(domains), len(returnedDomains))
	}

	for i, domain := range domains {
		if returnedDomains[i] != domain {
			t.Errorf("Domain mismatch at index %d: expected %s, got %s", i, domain, returnedDomains[i])
		}
	}
}
