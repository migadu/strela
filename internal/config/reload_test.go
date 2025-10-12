package config

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

func TestReloadableConfig_Reload(t *testing.T) {
	logger := slog.Default()

	// Create temporary config file
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.toml")

	// Write initial config
	initialConfig := `
[inbound]
listen = ":8080"
auth_token = "initial-token"
max_body_size_bytes = 10485760

[queue]
database_path = "./queue.db"
worker_count = 10
batch_size = 5

[outbound]
source_ips = ["192.168.1.100"]
source_ip_selection = "round-robin"
mx_cache_ttl_seconds = 3600
circuit_breaker_enabled = true
circuit_breaker_failure_threshold = 5

[callbacks]
webhook_url = "https://example.com/webhook"
timeout_seconds = 10
`
	if err := os.WriteFile(configPath, []byte(initialConfig), 0644); err != nil {
		t.Fatalf("failed to write initial config: %v", err)
	}

	// Create reloadable config
	rc, err := NewReloadableConfig(configPath, logger)
	if err != nil {
		t.Fatalf("failed to create reloadable config: %v", err)
	}

	// Verify initial config
	cfg := rc.Get()
	if cfg.Inbound.AuthToken != "initial-token" {
		t.Errorf("expected auth_token 'initial-token', got '%s'", cfg.Inbound.AuthToken)
	}
	if len(cfg.Outbound.SourceIPs) != 1 {
		t.Errorf("expected 1 source IP, got %d", len(cfg.Outbound.SourceIPs))
	}
	if cfg.Outbound.CircuitBreakerFailureThreshold != 5 {
		t.Errorf("expected threshold 5, got %d", cfg.Outbound.CircuitBreakerFailureThreshold)
	}

	// Write updated config (valid changes)
	updatedConfig := `
[inbound]
listen = ":8080"
auth_token = "updated-token"
max_body_size_bytes = 20971520

[queue]
database_path = "./queue.db"
worker_count = 10
batch_size = 5

[outbound]
source_ips = ["192.168.1.100", "192.168.1.101", "192.168.1.102"]
source_ip_selection = "random"
mx_cache_ttl_seconds = 7200
circuit_breaker_enabled = true
circuit_breaker_failure_threshold = 10

[callbacks]
webhook_url = "https://example.com/webhook"
timeout_seconds = 10
`
	if err := os.WriteFile(configPath, []byte(updatedConfig), 0644); err != nil {
		t.Fatalf("failed to write updated config: %v", err)
	}

	// Reload config
	if err := rc.Reload(); err != nil {
		t.Fatalf("reload failed: %v", err)
	}

	// Verify updated config
	cfg = rc.Get()
	if cfg.Inbound.AuthToken != "updated-token" {
		t.Errorf("expected auth_token 'updated-token', got '%s'", cfg.Inbound.AuthToken)
	}
	if len(cfg.Outbound.SourceIPs) != 3 {
		t.Errorf("expected 3 source IPs, got %d", len(cfg.Outbound.SourceIPs))
	}
	if cfg.Outbound.SourceIPSelection != "random" {
		t.Errorf("expected source_ip_selection 'random', got '%s'", cfg.Outbound.SourceIPSelection)
	}
	if cfg.Outbound.CircuitBreakerFailureThreshold != 10 {
		t.Errorf("expected threshold 10, got %d", cfg.Outbound.CircuitBreakerFailureThreshold)
	}
	if cfg.Inbound.MaxBodySizeBytes != 20971520 {
		t.Errorf("expected max_body_size 20971520, got %d", cfg.Inbound.MaxBodySizeBytes)
	}
}

func TestReloadableConfig_ReloadValidation(t *testing.T) {
	logger := slog.Default()

	tests := []struct {
		name          string
		initialConfig string
		updatedConfig string
		expectError   bool
		errorContains string
	}{
		{
			name: "database_path changed (should fail)",
			initialConfig: `
[server]
database_path = "./queue.db"
[inbound]
listen = ":8080"
[queue]
worker_count = 10
[outbound]
source_ips = []
[callbacks]
webhook_url = "https://example.com/webhook"
`,
			updatedConfig: `
[server]
database_path = "./queue-new.db"
[inbound]
listen = ":8080"
[queue]
worker_count = 10
[outbound]
source_ips = []
[callbacks]
webhook_url = "https://example.com/webhook"
`,
			expectError:   true,
			errorContains: "database_path cannot be changed",
		},
		{
			name: "listen address changed (should fail)",
			initialConfig: `
[inbound]
listen = ":8080"
[queue]
database_path = "./queue.db"
worker_count = 10
[outbound]
source_ips = []
[callbacks]
webhook_url = "https://example.com/webhook"
`,
			updatedConfig: `
[inbound]
listen = ":9090"
[queue]
database_path = "./queue.db"
worker_count = 10
[outbound]
source_ips = []
[callbacks]
webhook_url = "https://example.com/webhook"
`,
			expectError:   true,
			errorContains: "http.listen cannot be changed",
		},
		{
			name: "worker_count changed (should fail)",
			initialConfig: `
[inbound]
listen = ":8080"
[queue]
database_path = "./queue.db"
worker_count = 10
[outbound]
source_ips = []
[callbacks]
webhook_url = "https://example.com/webhook"
`,
			updatedConfig: `
[inbound]
listen = ":8080"
[queue]
database_path = "./queue.db"
worker_count = 20
[outbound]
source_ips = []
[callbacks]
webhook_url = "https://example.com/webhook"
`,
			expectError:   true,
			errorContains: "worker_count cannot be changed",
		},
		{
			name: "webhook_url changed (should fail)",
			initialConfig: `
[inbound]
listen = ":8080"
[queue]
database_path = "./queue.db"
worker_count = 10
[outbound]
source_ips = []
[callbacks]
webhook_url = "https://example.com/webhook"
`,
			updatedConfig: `
[inbound]
listen = ":8080"
[queue]
database_path = "./queue.db"
worker_count = 10
[outbound]
source_ips = []
[callbacks]
webhook_url = "https://different.com/webhook"
`,
			expectError:   true,
			errorContains: "webhook_url cannot be changed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmpDir := t.TempDir()
			configPath := filepath.Join(tmpDir, "config.toml")

			// Write initial config
			if err := os.WriteFile(configPath, []byte(tt.initialConfig), 0644); err != nil {
				t.Fatalf("failed to write initial config: %v", err)
			}

			// Create reloadable config
			rc, err := NewReloadableConfig(configPath, logger)
			if err != nil {
				t.Fatalf("failed to create reloadable config: %v", err)
			}

			// Write updated config
			if err := os.WriteFile(configPath, []byte(tt.updatedConfig), 0644); err != nil {
				t.Fatalf("failed to write updated config: %v", err)
			}

			// Attempt reload
			err = rc.Reload()

			if tt.expectError {
				if err == nil {
					t.Errorf("expected error containing '%s', got nil", tt.errorContains)
				} else if tt.errorContains != "" && !contains(err.Error(), tt.errorContains) {
					t.Errorf("expected error containing '%s', got '%s'", tt.errorContains, err.Error())
				}
			} else {
				if err != nil {
					t.Errorf("expected no error, got: %v", err)
				}
			}
		})
	}
}

func TestReloadableConfig_ReloadCallback(t *testing.T) {
	logger := slog.Default()

	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.toml")

	initialConfig := `
[inbound]
listen = ":8080"
[queue]
database_path = "./queue.db"
worker_count = 10
[outbound]
source_ips = ["192.168.1.100"]
[callbacks]
webhook_url = "https://example.com/webhook"
`
	if err := os.WriteFile(configPath, []byte(initialConfig), 0644); err != nil {
		t.Fatalf("failed to write config: %v", err)
	}

	rc, err := NewReloadableConfig(configPath, logger)
	if err != nil {
		t.Fatalf("failed to create reloadable config: %v", err)
	}

	// Register callback
	callbackCalled := false
	var callbackConfig *Config
	rc.RegisterReloadCallback(func(newCfg *Config) error {
		callbackCalled = true
		callbackConfig = newCfg
		return nil
	})

	// Update config
	updatedConfig := `
[inbound]
listen = ":8080"
[queue]
database_path = "./queue.db"
worker_count = 10
[outbound]
source_ips = ["192.168.1.100", "192.168.1.101"]
[callbacks]
webhook_url = "https://example.com/webhook"
`
	if err := os.WriteFile(configPath, []byte(updatedConfig), 0644); err != nil {
		t.Fatalf("failed to write updated config: %v", err)
	}

	// Reload
	if err := rc.Reload(); err != nil {
		t.Fatalf("reload failed: %v", err)
	}

	// Verify callback was called
	if !callbackCalled {
		t.Error("callback was not called")
	}

	// Verify callback received new config
	if callbackConfig == nil {
		t.Error("callback config is nil")
	} else if len(callbackConfig.Outbound.SourceIPs) != 2 {
		t.Errorf("expected 2 source IPs in callback, got %d", len(callbackConfig.Outbound.SourceIPs))
	}
}

func TestReloadableConfig_InvalidSyntax(t *testing.T) {
	logger := slog.Default()

	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.toml")

	validConfig := `
[inbound]
listen = ":8080"
[queue]
database_path = "./queue.db"
worker_count = 10
[outbound]
source_ips = []
[callbacks]
webhook_url = "https://example.com/webhook"
`
	if err := os.WriteFile(configPath, []byte(validConfig), 0644); err != nil {
		t.Fatalf("failed to write config: %v", err)
	}

	rc, err := NewReloadableConfig(configPath, logger)
	if err != nil {
		t.Fatalf("failed to create reloadable config: %v", err)
	}

	// Write invalid config (syntax error)
	invalidConfig := `
[inbound]
listen = ":8080"
invalid syntax here!
[queue]
`
	if err := os.WriteFile(configPath, []byte(invalidConfig), 0644); err != nil {
		t.Fatalf("failed to write invalid config: %v", err)
	}

	// Reload should fail
	err = rc.Reload()
	if err == nil {
		t.Error("expected error for invalid syntax, got nil")
	}

	// Old config should still be in use
	cfg := rc.Get()
	if cfg.Inbound.Listen != ":8080" {
		t.Errorf("config was changed despite reload failure")
	}
}

func TestReloadableConfig_GetMethods(t *testing.T) {
	logger := slog.Default()

	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.toml")

	configContent := `
[inbound]
listen = ":8080"
auth_token = "test-token"
[queue]
database_path = "./queue.db"
worker_count = 10
batch_size = 5
[outbound]
source_ips = ["192.168.1.100"]
source_ip_selection = "round-robin"
[callbacks]
webhook_url = "https://example.com/webhook"
timeout_seconds = 10
`
	if err := os.WriteFile(configPath, []byte(configContent), 0644); err != nil {
		t.Fatalf("failed to write config: %v", err)
	}

	rc, err := NewReloadableConfig(configPath, logger)
	if err != nil {
		t.Fatalf("failed to create reloadable config: %v", err)
	}

	// Test individual getter methods
	inboundCfg := rc.GetInbound()
	if inboundCfg.Listen != ":8080" {
		t.Errorf("GetInbound: expected listen ':8080', got '%s'", inboundCfg.Listen)
	}

	outboundCfg := rc.GetOutbound()
	if outboundCfg.SourceIPSelection != "round-robin" {
		t.Errorf("GetOutbound: expected source_ip_selection 'round-robin', got '%s'", outboundCfg.SourceIPSelection)
	}

	queueCfg := rc.GetQueue()
	if queueCfg.BatchSize != 5 {
		t.Errorf("GetQueue: expected batch_size 5, got %d", queueCfg.BatchSize)
	}

	callbacksCfg := rc.GetCallbacks()
	if callbacksCfg.TimeoutSeconds != 10 {
		t.Errorf("GetCallbacks: expected timeout 10, got %d", callbacksCfg.TimeoutSeconds)
	}
}

// Helper function
func contains(s, substr string) bool {
	return len(s) >= len(substr) && s[0:len(substr)] == substr || len(s) > len(substr) && contains(s[1:], substr)
}
