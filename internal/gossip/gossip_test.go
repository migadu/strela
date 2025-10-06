package gossip

import (
	"encoding/base64"
	"testing"
	"time"

	"go.uber.org/zap"
)

func TestGossipEncryptionKeyValidation(t *testing.T) {
	logger, _ := zap.NewDevelopment()

	tests := []struct {
		name        string
		secretKey   string
		expectError bool
		errorMsg    string
	}{
		{
			name:        "invalid base64",
			secretKey:   "not-valid-base64!@#$",
			expectError: true,
			errorMsg:    "failed to decode secret key",
		},
		{
			name:        "wrong key length (16 bytes)",
			secretKey:   base64.StdEncoding.EncodeToString(make([]byte, 16)),
			expectError: true,
			errorMsg:    "secret key must be exactly 32 bytes",
		},
		{
			name:        "wrong key length (64 bytes)",
			secretKey:   base64.StdEncoding.EncodeToString(make([]byte, 64)),
			expectError: true,
			errorMsg:    "secret key must be exactly 32 bytes",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &Config{
				Enabled:        true,
				BindPort:       0, // Random port
				SecretKey:      tt.secretKey,
				IdempotencyTTL: 24 * time.Hour,
			}

			_, err := NewGossip(cfg, logger)
			if tt.expectError {
				if err == nil {
					t.Errorf("Expected error containing '%s', but got nil", tt.errorMsg)
				} else if tt.errorMsg != "" && !contains(err.Error(), tt.errorMsg) {
					t.Errorf("Expected error containing '%s', got '%s'", tt.errorMsg, err.Error())
				}
			} else {
				if err != nil {
					t.Errorf("Expected no error, got: %v", err)
				}
			}
			// Don't start gossip service to avoid background goroutines
		})
	}
}

func TestGossipDisabled(t *testing.T) {
	logger, _ := zap.NewDevelopment()

	cfg := &Config{
		Enabled: false,
	}

	g, err := NewGossip(cfg, logger)
	if err != nil {
		t.Errorf("Expected no error when gossip is disabled, got: %v", err)
	}
	if g != nil {
		t.Error("Expected nil gossip instance when disabled")
	}
}

// Note: Additional gossip tests are skipped to avoid background goroutine
// issues during test execution. The gossip functionality is tested through
// integration tests and manual testing.

// Helper function to check if a string contains a substring
func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(substr) == 0 ||
		(len(s) > 0 && len(substr) > 0 && findSubstring(s, substr)))
}

func findSubstring(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
