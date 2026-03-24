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
max_total_delivery_seconds = 30
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

	if config.Outbound.MaxTotalDeliverySeconds != 30 {
		t.Errorf("Expected max_total_delivery_seconds 30, got %d", config.Outbound.MaxTotalDeliverySeconds)
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

	if config.Outbound.BannerTimeoutSeconds != 30 {
		t.Errorf("Expected default banner_timeout_seconds 30, got %d", config.Outbound.BannerTimeoutSeconds)
	}

	if config.Outbound.HandshakeTimeoutSeconds != 30 {
		t.Errorf("Expected default handshake_timeout_seconds 30, got %d", config.Outbound.HandshakeTimeoutSeconds)
	}

	if config.Outbound.MaxTotalDeliverySeconds != 200 {
		t.Errorf("Expected default max_total_delivery_seconds 200, got %d", config.Outbound.MaxTotalDeliverySeconds)
	}

	if config.Outbound.SourceIPSelection != "round-robin" {
		t.Errorf("Expected default source_ip_selection round-robin, got %s", config.Outbound.SourceIPSelection)
	}

	if config.Outbound.PerDomainIntervalSeconds != 2 {
		t.Errorf("Expected default per_domain_interval_seconds 2, got %d", config.Outbound.PerDomainIntervalSeconds)
	}

	// Check HTTP timeout defaults (v2.0.7 updated to accommodate longer delivery timeout)
	if config.Inbound.ReadTimeoutSecs != 30 {
		t.Errorf("Expected default read_timeout_seconds 30, got %d", config.Inbound.ReadTimeoutSecs)
	}

	if config.Inbound.WriteTimeoutSecs != 240 {
		t.Errorf("Expected default write_timeout_seconds 240, got %d", config.Inbound.WriteTimeoutSecs)
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

	// Verify defaults were applied (v2.0.7 updated defaults)
	if config.Outbound.MaxTotalDeliverySeconds != 200 {
		t.Errorf("Expected default max_total_delivery_seconds 200, got %d", config.Outbound.MaxTotalDeliverySeconds)
	}

	if config.Outbound.ConnectionTimeoutSeconds != 15 {
		t.Errorf("Expected default connection_timeout_seconds 15, got %d", config.Outbound.ConnectionTimeoutSeconds)
	}

	if config.Outbound.BannerTimeoutSeconds != 30 {
		t.Errorf("Expected default banner_timeout_seconds 30, got %d", config.Outbound.BannerTimeoutSeconds)
	}

	if config.Outbound.HandshakeTimeoutSeconds != 30 {
		t.Errorf("Expected default handshake_timeout_seconds 30, got %d", config.Outbound.HandshakeTimeoutSeconds)
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

func TestValidate_SMTP(t *testing.T) {
	cfg := &Config{
		Outbound: OutboundConfig{
			Protocol: "smtp",
		},
	}

	if err := cfg.Validate(); err != nil {
		t.Errorf("Expected no error for SMTP protocol, got: %v", err)
	}
}

func TestValidate_LMTP_Valid(t *testing.T) {
	cfg := &Config{
		Outbound: OutboundConfig{
			Protocol:        "lmtp",
			LMTPDestination: "lmtp.example.com:24",
		},
	}

	if err := cfg.Validate(); err != nil {
		t.Errorf("Expected no error for valid LMTP config, got: %v", err)
	}
}

func TestValidate_LMTP_MissingDestination(t *testing.T) {
	cfg := &Config{
		Outbound: OutboundConfig{
			Protocol: "lmtp",
		},
	}

	err := cfg.Validate()
	if err == nil {
		t.Error("Expected error for LMTP without destination, got nil")
	}
	expectedMsg := "outbound.lmtp_destination is required when protocol is 'lmtp'"
	if err.Error() != expectedMsg {
		t.Errorf("Expected error message '%s', got: %v", expectedMsg, err)
	}
}

func TestValidate_LMTP_InvalidHostPort(t *testing.T) {
	tests := []struct {
		name        string
		destination string
		wantErr     bool
	}{
		{"missing_port", "lmtp.example.com", true},
		{"missing_host", ":24", true},
		{"empty", "", true},
		{"valid", "lmtp.example.com:24", false},
		{"valid_ipv4", "192.0.2.1:24", false},
		{"valid_ipv6", "[2001:db8::1]:24", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &Config{
				Outbound: OutboundConfig{
					Protocol:        "lmtp",
					LMTPDestination: tt.destination,
				},
			}

			err := cfg.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestValidate_InvalidProtocol(t *testing.T) {
	cfg := &Config{
		Outbound: OutboundConfig{
			Protocol: "invalid",
		},
	}

	err := cfg.Validate()
	if err == nil {
		t.Error("Expected error for invalid protocol, got nil")
	}
	expectedMsg := "outbound.protocol must be 'smtp' or 'lmtp', got: invalid"
	if err.Error() != expectedMsg {
		t.Errorf("Expected error message '%s', got: %v", expectedMsg, err)
	}
}

func TestSetDefaults_Protocol(t *testing.T) {
	cfg := &Config{}
	cfg.SetDefaults()

	if cfg.Outbound.Protocol != "smtp" {
		t.Errorf("Expected default protocol 'smtp', got: %s", cfg.Outbound.Protocol)
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
			input:    "strela/prod",
			expected: "strela/prod/",
		},
		{
			name:     "nested prefix with trailing slash",
			input:    "strela/prod/",
			expected: "strela/prod/",
		},
		{
			name:     "prefix with leading slash",
			input:    "/myapp",
			expected: "myapp/",
		},
		{
			name:     "prefix with leading and trailing slash",
			input:    "/myapp/",
			expected: "myapp/",
		},
		{
			name:     "prefix with multiple leading slashes",
			input:    "///myapp",
			expected: "myapp/",
		},
		{
			name:     "nested prefix with leading slash",
			input:    "/strela/prod",
			expected: "strela/prod/",
		},
		{
			name:     "only slashes becomes empty",
			input:    "///",
			expected: "",
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
