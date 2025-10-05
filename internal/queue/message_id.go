package queue

import (
	"crypto/rand"
	"encoding/base32"
	"fmt"
	"strings"
	"time"
)

// GenerateMessageID creates a unique message ID for tracking
// Format: msg_<timestamp><random> (e.g., msg_2f4h3k9d2j8x)
func GenerateMessageID() string {
	// Use timestamp + random bytes for uniqueness
	timestamp := time.Now().Unix()

	// 8 random bytes for collision resistance
	randomBytes := make([]byte, 8)
	rand.Read(randomBytes)

	// Base32 encoding (lowercase, no padding) for URL-safe IDs
	encoder := base32.StdEncoding.WithPadding(base32.NoPadding)
	randomPart := strings.ToLower(encoder.EncodeToString(randomBytes))

	return fmt.Sprintf("msg_%x%s", timestamp, randomPart[:10])
}

// Example outputs:
// msg_679d8a4c2f4h3k9d2j
// msg_679d8a4dkl3m5n7p9q
