package config

import (
	"os"
	"testing"
)

func TestLoadConfig(t *testing.T) {
	// Create temporary config file
	configContent := `
[inbound]
listen = ":8080"
auth_token = "test-token"
max_concurrent_requests = 100

[outbound]
source_ips_v4 = ["192.168.1.100", "192.168.1.101"]
source_ip_selection = "round-robin"
delivery_timeout_seconds = 30
`

	tmpFile, err := os.CreateTemp("", "config_*.toml")
	if err != nil {
		t.Fatalf("Failed to create temp file: %v", err)
	}
	defer os.Remove(tmpFile.Name())

	if _, err := tmpFile.WriteString(configContent); err != nil {
		t.Fatalf("Failed to write config: %v", err)
	}
	tmpFile.Close()

	// Load config
	config, err := LoadConfig(tmpFile.Name())
	if err != nil {
		t.Fatalf("Failed to load config: %v", err)
	}

	// Verify values
	if config.Inbound.Listen != ":8080" {
		t.Errorf("Expected listen :8080, got %s", config.Inbound.Listen)
	}

	if config.Inbound.AuthToken != "test-token" {
		t.Errorf("Expected auth_token test-token, got %s", config.Inbound.AuthToken)
	}

	if config.Inbound.MaxConcurrentRequests != 100 {
		t.Errorf("Expected max_concurrent_requests 100, got %d", config.Inbound.MaxConcurrentRequests)
	}

	if len(config.Outbound.SourceIPsV4) != 2 {
		t.Errorf("Expected 2 source IPv4s, got %d", len(config.Outbound.SourceIPsV4))
	}

	if config.Outbound.DeliveryTimeoutSeconds != 30 {
		t.Errorf("Expected delivery_timeout_seconds 30, got %d", config.Outbound.DeliveryTimeoutSeconds)
	}
}

func TestSetDefaults(t *testing.T) {
	config := &Config{}
	config.SetDefaults()

	// Check defaults
	if config.Outbound.MXCacheTTLSeconds != 3600 {
		t.Errorf("Expected default mx_cache_ttl_seconds 3600, got %d", config.Outbound.MXCacheTTLSeconds)
	}

	if config.Outbound.ConnectionTimeoutSeconds != 15 {
		t.Errorf("Expected default connection_timeout_seconds 15, got %d", config.Outbound.ConnectionTimeoutSeconds)
	}

	if config.Outbound.SMTPTimeoutSeconds != 60 {
		t.Errorf("Expected default smtp_timeout_seconds 60, got %d", config.Outbound.SMTPTimeoutSeconds)
	}

	if config.Outbound.DeliveryTimeoutSeconds != 30 {
		t.Errorf("Expected default delivery_timeout_seconds 30, got %d", config.Outbound.DeliveryTimeoutSeconds)
	}

	if config.Outbound.SourceIPSelection != "round-robin" {
		t.Errorf("Expected default source_ip_selection round-robin, got %s", config.Outbound.SourceIPSelection)
	}

	if config.Outbound.PerDomainIntervalSeconds != 2 {
		t.Errorf("Expected default per_domain_interval_seconds 2, got %d", config.Outbound.PerDomainIntervalSeconds)
	}

	// Check HTTP timeout defaults
	if config.Inbound.ReadTimeoutSecs != 30 {
		t.Errorf("Expected default read_timeout_seconds 30, got %d", config.Inbound.ReadTimeoutSecs)
	}

	if config.Inbound.WriteTimeoutSecs != 90 {
		t.Errorf("Expected default write_timeout_seconds 90, got %d", config.Inbound.WriteTimeoutSecs)
	}

	if config.Inbound.IdleTimeoutSecs != 120 {
		t.Errorf("Expected default idle_timeout_seconds 120, got %d", config.Inbound.IdleTimeoutSecs)
	}

	if config.Inbound.MaxBodySizeBytes != 35*1024*1024 {
		t.Errorf("Expected default max_body_size_bytes 35MB, got %d", config.Inbound.MaxBodySizeBytes)
	}

	if config.Logging.Level != "info" {
		t.Errorf("Expected default log level info, got %s", config.Logging.Level)
	}

	if config.Logging.Format != "console" {
		t.Errorf("Expected default log format console, got %s", config.Logging.Format)
	}
}

func TestLoadConfigWithDefaults(t *testing.T) {
	// Create minimal config file
	configContent := `
[inbound]
listen = ":8080"

[outbound]
source_ips = ["192.168.1.100"]
`

	tmpFile, err := os.CreateTemp("", "config_minimal_*.toml")
	if err != nil {
		t.Fatalf("Failed to create temp file: %v", err)
	}
	defer os.Remove(tmpFile.Name())

	if _, err := tmpFile.WriteString(configContent); err != nil {
		t.Fatalf("Failed to write config: %v", err)
	}
	tmpFile.Close()

	// Load config
	config, err := LoadConfig(tmpFile.Name())
	if err != nil {
		t.Fatalf("Failed to load config: %v", err)
	}

	// Verify defaults were applied
	if config.Outbound.DeliveryTimeoutSeconds != 30 {
		t.Errorf("Expected default delivery_timeout_seconds 30, got %d", config.Outbound.DeliveryTimeoutSeconds)
	}

	if config.Outbound.ConnectionTimeoutSeconds != 15 {
		t.Errorf("Expected default connection_timeout_seconds 15, got %d", config.Outbound.ConnectionTimeoutSeconds)
	}

	if config.Inbound.MaxConcurrentRequests != 0 {
		t.Errorf("Expected default max_concurrent_requests 0 (unlimited), got %d", config.Inbound.MaxConcurrentRequests)
	}
}

func TestLoadConfigInvalidPath(t *testing.T) {
	_, err := LoadConfig("/nonexistent/path/config.toml")
	if err == nil {
		t.Error("Expected error for nonexistent config file, got nil")
	}
}

func TestLoadConfigInvalidTOML(t *testing.T) {
	// Create invalid TOML file
	tmpFile, err := os.CreateTemp("", "config_invalid_*.toml")
	if err != nil {
		t.Fatalf("Failed to create temp file: %v", err)
	}
	defer os.Remove(tmpFile.Name())

	if _, err := tmpFile.WriteString("invalid toml [[["); err != nil {
		t.Fatalf("Failed to write config: %v", err)
	}
	tmpFile.Close()

	_, err = LoadConfig(tmpFile.Name())
	if err == nil {
		t.Error("Expected error for invalid TOML, got nil")
	}
}

func TestS3PrefixNormalization(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "empty prefix",
			input:    "",
			expected: "",
		},
		{
			name:     "prefix without trailing slash",
			input:    "myapp",
			expected: "myapp/",
		},
		{
			name:     "prefix with trailing slash",
			input:    "myapp/",
			expected: "myapp/",
		},
		{
			name:     "nested prefix without trailing slash",
			input:    "fune/prod",
			expected: "fune/prod/",
		},
		{
			name:     "nested prefix with trailing slash",
			input:    "fune/prod/",
			expected: "fune/prod/",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &Config{}
			cfg.TLS.LetsEncrypt.S3.Prefix = tt.input
			cfg.SetDefaults()

			if cfg.TLS.LetsEncrypt.S3.Prefix != tt.expected {
				t.Errorf("Expected prefix '%s', got '%s'", tt.expected, cfg.TLS.LetsEncrypt.S3.Prefix)
			}
		})
	}
}
