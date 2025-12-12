// Package srs implements Sender Rewriting Scheme (SRS) for envelope sender rewriting.
//
// SRS rewrites the envelope sender (MAIL FROM) address when forwarding email to prevent
// SPF (Sender Policy Framework) failures. When forwarding email, the original sender's
// domain won't authorize the forwarding server's IP addresses, causing SPF validation
// to fail at the final destination. SRS solves this by rewriting the envelope sender
// to use the forwarding server's domain while preserving the original sender information.
//
// Key Features:
//   - SRS0 format: SRS0=hash=timestamp=domain=localpart@srsdomain
//   - SRS1 format: SRS1=hash=domain=hash=timestamp=domain=localpart@srsdomain (for re-forwarding)
//   - HMAC-based hash for validation and security
//   - Configurable hash length (2-8 characters)
//   - Time-based expiration to prevent replay attacks
//   - Base32 encoding for timestamp (compact representation)
//
// SRS Format:
//
// The SRS0 format (first forward):
//
//	Original: user@example.com
//	Rewritten: SRS0=HASH=TT=example.com=user@forwarding.com
//
//	Where:
//	  HASH = First N characters of HMAC-SHA1(secret + timestamp + domain + localpart)
//	  TT = Base32-encoded timestamp (days since epoch / 4)
//	  example.com = Original sender domain
//	  user = Original sender local part
//	  forwarding.com = SRS domain (forwarding server's domain)
//
// The SRS1 format (re-forwarding):
//
//	When an SRS0 address is forwarded again, it becomes SRS1 to prevent
//	unbounded address length growth.
//
// Bounce Handling:
//
// When a bounce occurs, it will be sent to the SRS address. The receiving server
// can decode the SRS address to determine the original sender and route the bounce
// appropriately.
//
// Security Considerations:
//
//   - Use a strong secret (min 16 characters, ideally random)
//   - Keep the secret secure and rotate periodically
//   - Set appropriate max_age to limit address validity window
//   - Hash prevents address forgery (requires knowledge of secret)
//
// References:
//   - https://en.wikipedia.org/wiki/Sender_Rewriting_Scheme
//   - https://www.libsrs2.org/
package srs

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base32"
	"fmt"
	"strings"
	"time"
)

// SRS implements Sender Rewriting Scheme for envelope sender rewriting
type SRS struct {
	domain        string // SRS domain for rewritten addresses
	secret        string // Secret for HMAC hash generation
	maxAge        int    // Maximum age in days for SRS addresses
	hashLength    int    // Length of hash in characters (2-8)
	separator     string // Separator character (default "=")
	alwaysRewrite bool   // Always rewrite, even non-forwarded addresses
}

// NewSRS creates a new SRS instance with the specified configuration
func NewSRS(domain, secret string, maxAge, hashLength int, separator string, alwaysRewrite bool) (*SRS, error) {
	if domain == "" {
		return nil, fmt.Errorf("SRS domain cannot be empty")
	}
	if secret == "" {
		return nil, fmt.Errorf("SRS secret cannot be empty")
	}
	if len(secret) < 16 {
		return nil, fmt.Errorf("SRS secret must be at least 16 characters (got %d)", len(secret))
	}
	if maxAge <= 0 {
		maxAge = 21 // Default 21 days
	}
	if hashLength < 2 || hashLength > 8 {
		return nil, fmt.Errorf("SRS hash length must be between 2 and 8 (got %d)", hashLength)
	}
	if separator == "" {
		separator = "="
	}

	return &SRS{
		domain:        strings.ToLower(domain),
		secret:        secret,
		maxAge:        maxAge,
		hashLength:    hashLength,
		separator:     separator,
		alwaysRewrite: alwaysRewrite,
	}, nil
}

// Forward rewrites an email address to SRS format (SRS0)
//
// Parameters:
//   - sender: Original sender email address (e.g., "user@example.com")
//
// # Returns the rewritten SRS address or an error if the address is invalid
//
// Example:
//
//	srs.Forward("user@example.com")
//	// Returns: "SRS0=ABCD=TT=example.com=user@forwarding.com"
func (s *SRS) Forward(sender string) (string, error) {
	if sender == "" {
		return "", fmt.Errorf("sender address cannot be empty")
	}

	// Parse sender address
	localpart, domain, err := parseEmail(sender)
	if err != nil {
		return "", fmt.Errorf("invalid sender address: %w", err)
	}

	// Check if already SRS-encoded
	if strings.HasPrefix(strings.ToUpper(localpart), "SRS0"+s.separator) {
		// Already SRS0, convert to SRS1 to prevent unbounded growth
		return s.forwardSRS1(localpart, domain)
	}
	if strings.HasPrefix(strings.ToUpper(localpart), "SRS1"+s.separator) {
		// Already SRS1, keep as SRS1
		return sender, nil
	}

	// Create SRS0 address
	return s.forwardSRS0(localpart, domain)
}

// forwardSRS0 creates an SRS0 address from a regular email address
func (s *SRS) forwardSRS0(localpart, domain string) (string, error) {
	// Generate timestamp (base32-encoded days since epoch / 4)
	timestamp := s.generateTimestamp()

	// Generate hash
	hash := s.generateHash(timestamp, domain, localpart)

	// Build SRS0 address: SRS0=hash=timestamp=domain=localpart@srsdomain
	srsLocal := fmt.Sprintf("SRS0%s%s%s%s%s%s%s%s",
		s.separator, hash,
		s.separator, timestamp,
		s.separator, domain,
		s.separator, localpart)

	return srsLocal + "@" + s.domain, nil
}

// forwardSRS1 creates an SRS1 address from an SRS0 address (for re-forwarding)
func (s *SRS) forwardSRS1(srs0Localpart, srs0Domain string) (string, error) {
	// SRS1 format: SRS1=hash=srsdomain=rest@newsrsdomain
	// Extract the SRS0 parts
	parts := strings.Split(srs0Localpart, s.separator)
	if len(parts) < 2 {
		return "", fmt.Errorf("invalid SRS0 format")
	}

	// Generate new hash for SRS1
	hash := s.generateHash(srs0Domain, srs0Localpart, "")

	// Build SRS1 address
	srsLocal := fmt.Sprintf("SRS1%s%s%s%s%s%s",
		s.separator, hash,
		s.separator, srs0Domain,
		s.separator, srs0Localpart)

	return srsLocal + "@" + s.domain, nil
}

// Reverse decodes an SRS address back to the original sender address
//
// Parameters:
//   - srsAddress: SRS-encoded email address
//
// Returns the original sender address or an error if:
//   - Address is not SRS-encoded
//   - Hash validation fails
//   - Address has expired
//
// Example:
//
//	srs.Reverse("SRS0=ABCD=TT=example.com=user@forwarding.com")
//	// Returns: "user@example.com"
func (s *SRS) Reverse(srsAddress string) (string, error) {
	if srsAddress == "" {
		return "", fmt.Errorf("SRS address cannot be empty")
	}

	// Parse SRS address
	localpart, _, err := parseEmail(srsAddress)
	if err != nil {
		return "", fmt.Errorf("invalid SRS address: %w", err)
	}

	localpartUpper := strings.ToUpper(localpart)

	// Check for SRS0
	if strings.HasPrefix(localpartUpper, "SRS0"+s.separator) {
		return s.reverseSRS0(localpart)
	}

	// Check for SRS1
	if strings.HasPrefix(localpartUpper, "SRS1"+s.separator) {
		return s.reverseSRS1(localpart)
	}

	return "", fmt.Errorf("not an SRS address")
}

// reverseSRS0 decodes an SRS0 address
func (s *SRS) reverseSRS0(localpart string) (string, error) {
	// SRS0=hash=timestamp=domain=localpart
	parts := strings.SplitN(localpart, s.separator, 5)
	if len(parts) != 5 {
		return "", fmt.Errorf("invalid SRS0 format: expected 5 parts, got %d", len(parts))
	}

	srsType := parts[0]
	hash := parts[1]
	timestamp := parts[2]
	domain := parts[3]
	origLocalpart := parts[4]

	if strings.ToUpper(srsType) != "SRS0" {
		return "", fmt.Errorf("expected SRS0, got %s", srsType)
	}

	// Validate timestamp
	if err := s.validateTimestamp(timestamp); err != nil {
		return "", fmt.Errorf("timestamp validation failed: %w", err)
	}

	// Validate hash
	expectedHash := s.generateHash(timestamp, domain, origLocalpart)
	if !hmac.Equal([]byte(hash), []byte(expectedHash)) {
		return "", fmt.Errorf("hash validation failed: signature mismatch")
	}

	// Reconstruct original address
	return origLocalpart + "@" + domain, nil
}

// reverseSRS1 decodes an SRS1 address
func (s *SRS) reverseSRS1(localpart string) (string, error) {
	// SRS1=hash=domain=srs0localpart
	parts := strings.SplitN(localpart, s.separator, 4)
	if len(parts) != 4 {
		return "", fmt.Errorf("invalid SRS1 format: expected 4 parts, got %d", len(parts))
	}

	srsType := parts[0]
	hash := parts[1]
	domain := parts[2]
	srs0Localpart := parts[3]

	if strings.ToUpper(srsType) != "SRS1" {
		return "", fmt.Errorf("expected SRS1, got %s", srsType)
	}

	// Validate hash
	expectedHash := s.generateHash(domain, srs0Localpart, "")
	if !hmac.Equal([]byte(hash), []byte(expectedHash)) {
		return "", fmt.Errorf("hash validation failed: signature mismatch")
	}

	// Reconstruct SRS0 address and reverse it
	srs0Address := srs0Localpart + "@" + domain
	return s.Reverse(srs0Address)
}

// IsSRS checks if an email address is SRS-encoded
func (s *SRS) IsSRS(address string) bool {
	localpart, _, err := parseEmail(address)
	if err != nil {
		return false
	}

	localpartUpper := strings.ToUpper(localpart)
	return strings.HasPrefix(localpartUpper, "SRS0"+s.separator) ||
		strings.HasPrefix(localpartUpper, "SRS1"+s.separator)
}

// generateHash creates an HMAC-SHA1 hash truncated to hashLength characters
func (s *SRS) generateHash(parts ...string) string {
	// Concatenate all parts
	data := strings.Join(parts, s.separator)

	// Generate HMAC-SHA1
	h := hmac.New(sha1.New, []byte(s.secret))
	h.Write([]byte(data))
	sum := h.Sum(nil)

	// Convert to base32 and truncate
	encoded := base32.StdEncoding.EncodeToString(sum)
	encoded = strings.ToUpper(strings.TrimRight(encoded, "="))

	if len(encoded) > s.hashLength {
		return encoded[:s.hashLength]
	}
	return encoded
}

// generateTimestamp creates a base32-encoded timestamp
// Uses a compact 2-character representation that covers multiple days
func (s *SRS) generateTimestamp() string {
	// Calculate days since Unix epoch
	daysSinceEpoch := int(time.Now().Unix() / 86400)

	// Use modulo to create a rolling timestamp (wraps every ~3 years with base32)
	// This prevents timestamp from growing indefinitely while still allowing validation
	timeCode := daysSinceEpoch % 1024 // 1024 gives us 2 base32 chars (5 bits * 2 = 10 bits)

	// Convert to base32 - take only the characters we need
	chars := "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567"

	// Encode 10 bits as 2 base32 characters
	char1 := chars[(timeCode>>5)&0x1F] // Upper 5 bits
	char2 := chars[timeCode&0x1F]      // Lower 5 bits

	return string([]byte{char1, char2})
}

// validateTimestamp checks if a timestamp is within the valid age range
func (s *SRS) validateTimestamp(timestamp string) error {
	if len(timestamp) != 2 {
		return fmt.Errorf("timestamp must be exactly 2 characters, got %d", len(timestamp))
	}

	// Decode the 2-character base32 timestamp
	chars := "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567"

	// Convert to uppercase for case-insensitive matching
	timestampUpper := strings.ToUpper(timestamp)

	// Find index of each character
	idx1 := strings.IndexByte(chars, timestampUpper[0])
	idx2 := strings.IndexByte(chars, timestampUpper[1])

	if idx1 == -1 || idx2 == -1 {
		return fmt.Errorf("invalid timestamp characters: %s", timestamp)
	}

	// Reconstruct the time code (10 bits)
	addressTimeCode := (idx1 << 5) | idx2

	// Get current time code
	currentDays := int(time.Now().Unix() / 86400)
	currentTimeCode := currentDays % 1024

	// Calculate age, accounting for wraparound
	var ageDays int
	if currentTimeCode >= addressTimeCode {
		ageDays = currentTimeCode - addressTimeCode
	} else {
		// Wraparound occurred
		ageDays = (1024 - addressTimeCode) + currentTimeCode
	}

	// Check if within valid range
	if ageDays > s.maxAge {
		return fmt.Errorf("timestamp expired: ~%d days old (max: %d)", ageDays, s.maxAge)
	}

	return nil
}

// parseEmail splits an email address into localpart and domain
func parseEmail(email string) (localpart, domain string, err error) {
	email = strings.TrimSpace(email)

	// Remove angle brackets if present
	email = strings.Trim(email, "<>")

	// Count @ symbols
	atCount := strings.Count(email, "@")
	if atCount == 0 {
		return "", "", fmt.Errorf("missing @ symbol")
	}
	if atCount > 1 {
		return "", "", fmt.Errorf("multiple @ symbols found")
	}

	// Find @ symbol
	atIndex := strings.Index(email, "@")
	if atIndex == 0 {
		return "", "", fmt.Errorf("empty local part")
	}
	if atIndex == len(email)-1 {
		return "", "", fmt.Errorf("empty domain")
	}

	localpart = email[:atIndex]
	domain = email[atIndex+1:]

	// Only lowercase the domain, keep localpart as-is for SRS encoding
	return localpart, strings.ToLower(domain), nil
}

// GetDomain returns the SRS domain
func (s *SRS) GetDomain() string {
	return s.domain
}
