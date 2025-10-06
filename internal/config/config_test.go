package config

import (
	"os"
	"testing"
)

func TestLoadConfig(t *testing.T) {
	// Create temporary config file
	configContent := `
[server]
database_path = "./test.db"

[inbound]
listen = ":8080"
auth_token = "test-token"

[queue]
worker_count = 5

[outbound]
source_ips = ["192.168.1.100", "192.168.1.101"]
ip_selection = "round-robin"

[callbacks]
webhook_url = "https://example.com/webhook"
auth_token = "webhook-token"
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

	if config.Server.DatabasePath != "./test.db" {
		t.Errorf("Expected database_path ./test.db, got %s", config.Server.DatabasePath)
	}

	if config.Queue.WorkerCount != 5 {
		t.Errorf("Expected worker_count 5, got %d", config.Queue.WorkerCount)
	}

	if len(config.Outbound.SourceIPs) != 2 {
		t.Errorf("Expected 2 source IPs, got %d", len(config.Outbound.SourceIPs))
	}

	if config.Callbacks.WebhookURL != "https://example.com/webhook" {
		t.Errorf("Expected webhook_url https://example.com/webhook, got %s", config.Callbacks.WebhookURL)
	}
}

func TestSetDefaults(t *testing.T) {
	config := &Config{}
	config.SetDefaults()

	// Check defaults
	if config.Queue.WorkerCount != 10 {
		t.Errorf("Expected default worker_count 10, got %d", config.Queue.WorkerCount)
	}

	if config.Queue.BatchSize != 5 {
		t.Errorf("Expected default batch_size 5, got %d", config.Queue.BatchSize)
	}

	if config.Queue.CleanupIntervalSeconds != 60 {
		t.Errorf("Expected default cleanup_interval_seconds 60, got %d", config.Queue.CleanupIntervalSeconds)
	}

	if config.Outbound.MXCacheTTLSeconds != 3600 {
		t.Errorf("Expected default mx_cache_ttl_seconds 3600, got %d", config.Outbound.MXCacheTTLSeconds)
	}

	if config.Outbound.ConnectionTimeoutSeconds != 30 {
		t.Errorf("Expected default connection_timeout_seconds 30, got %d", config.Outbound.ConnectionTimeoutSeconds)
	}

	if config.Outbound.MaxMessageAgeHours != 48 {
		t.Errorf("Expected default max_message_age_hours 48, got %d", config.Outbound.MaxMessageAgeHours)
	}

	if config.Outbound.InitialRetryDelaySeconds != 300 {
		t.Errorf("Expected default initial_retry_delay_seconds 300, got %d", config.Outbound.InitialRetryDelaySeconds)
	}

	if config.Outbound.MaxRetryDelaySeconds != 43200 {
		t.Errorf("Expected default max_retry_delay_seconds 43200, got %d", config.Outbound.MaxRetryDelaySeconds)
	}

	if config.Outbound.BackoffMultiplier != 2.0 {
		t.Errorf("Expected default backoff_multiplier 2.0, got %f", config.Outbound.BackoffMultiplier)
	}

	if config.Outbound.GreylistRetryDelaySeconds != 120 {
		t.Errorf("Expected default greylist_retry_delay_seconds 120, got %d", config.Outbound.GreylistRetryDelaySeconds)
	}

	if config.Outbound.SourceIPSelection != "round-robin" {
		t.Errorf("Expected default source_ip_selection round-robin, got %s", config.Outbound.SourceIPSelection)
	}

	if config.Callbacks.TimeoutSeconds != 10 {
		t.Errorf("Expected default timeout_seconds 10, got %d", config.Callbacks.TimeoutSeconds)
	}

	if config.Callbacks.MaxCallbackAgeHours != 48 {
		t.Errorf("Expected default max_callback_age_hours 48, got %d", config.Callbacks.MaxCallbackAgeHours)
	}

	if config.Callbacks.InitialRetryDelaySeconds != 30 {
		t.Errorf("Expected default initial_retry_delay_seconds 30, got %d", config.Callbacks.InitialRetryDelaySeconds)
	}

	if config.Callbacks.MaxRetryDelaySeconds != 3600 {
		t.Errorf("Expected default max_retry_delay_seconds 3600, got %d", config.Callbacks.MaxRetryDelaySeconds)
	}

	if config.Callbacks.BackoffMultiplier != 2.0 {
		t.Errorf("Expected default backoff_multiplier 2.0, got %f", config.Callbacks.BackoffMultiplier)
	}

	if config.Callbacks.BatchSize != 10 {
		t.Errorf("Expected default callback batch_size 10, got %d", config.Callbacks.BatchSize)
	}

	// Check HTTP timeout defaults
	if config.Inbound.ReadTimeoutSecs != 30 {
		t.Errorf("Expected default read_timeout_seconds 30, got %d", config.Inbound.ReadTimeoutSecs)
	}

	if config.Inbound.WriteTimeoutSecs != 30 {
		t.Errorf("Expected default write_timeout_seconds 30, got %d", config.Inbound.WriteTimeoutSecs)
	}

	if config.Inbound.IdleTimeoutSecs != 120 {
		t.Errorf("Expected default idle_timeout_seconds 120, got %d", config.Inbound.IdleTimeoutSecs)
	}

	if config.Inbound.MaxBodySizeBytes != 35*1024*1024 {
		t.Errorf("Expected default max_body_size_bytes 35MB, got %d", config.Inbound.MaxBodySizeBytes)
	}
}

func TestLoadConfigWithDefaults(t *testing.T) {
	// Create minimal config file
	configContent := `
[inbound]
listen = ":8080"

[queue]
database_path = "./test.db"

[outbound]
source_ips = ["192.168.1.100"]

[callbacks]
webhook_url = "https://example.com/webhook"
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
	if config.Queue.WorkerCount != 10 {
		t.Errorf("Expected default worker_count 10, got %d", config.Queue.WorkerCount)
	}

	if config.Outbound.MaxMessageAgeHours != 48 {
		t.Errorf("Expected default max_message_age_hours 48, got %d", config.Outbound.MaxMessageAgeHours)
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
