package config

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
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
max_concurrent_requests = 100

[outbound]
source_ips_v4 = ["192.168.1.100"]
source_ip_selection = "round-robin"
mx_cache_ttl_seconds = 3600
max_total_delivery_seconds = 30
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
	if len(cfg.Outbound.SourceIPsV4) != 1 {
		t.Errorf("expected 1 source IPv4, got %d", len(cfg.Outbound.SourceIPsV4))
	}
	if cfg.Inbound.MaxConcurrentRequests != 100 {
		t.Errorf("expected max_concurrent_requests 100, got %d", cfg.Inbound.MaxConcurrentRequests)
	}

	// Write updated config (valid changes)
	updatedConfig := `
[inbound]
listen = ":8080"
auth_token = "updated-token"
max_body_size_bytes = 20971520
max_concurrent_requests = 200

[outbound]
source_ips_v4 = ["192.168.1.100", "192.168.1.101", "192.168.1.102"]
source_ip_selection = "random"
mx_cache_ttl_seconds = 7200
max_total_delivery_seconds = 60
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
	if len(cfg.Outbound.SourceIPsV4) != 3 {
		t.Errorf("expected 3 source IPv4s, got %d", len(cfg.Outbound.SourceIPsV4))
	}
	if cfg.Outbound.SourceIPSelection != "random" {
		t.Errorf("expected source_ip_selection 'random', got '%s'", cfg.Outbound.SourceIPSelection)
	}
	if cfg.Inbound.MaxBodySizeBytes != 20971520 {
		t.Errorf("expected max_body_size 20971520, got %d", cfg.Inbound.MaxBodySizeBytes)
	}
	if cfg.Inbound.MaxConcurrentRequests != 200 {
		t.Errorf("expected max_concurrent_requests 200, got %d", cfg.Inbound.MaxConcurrentRequests)
	}
	if cfg.Outbound.MaxTotalDeliverySeconds != 60 {
		t.Errorf("expected max_total_delivery_seconds 60, got %d", cfg.Outbound.MaxTotalDeliverySeconds)
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
			name: "listen address changed (should fail)",
			initialConfig: `
[inbound]
listen = ":8080"
[outbound]
source_ips = []
`,
			updatedConfig: `
[inbound]
listen = ":9090"
[outbound]
source_ips = []
`,
			expectError:   true,
			errorContains: "http.listen cannot be changed",
		},
		{
			name: "source IPs changed (should succeed)",
			initialConfig: `
[inbound]
listen = ":8080"
[outbound]
source_ips = ["192.168.1.100"]
`,
			updatedConfig: `
[inbound]
listen = ":8080"
[outbound]
source_ips = ["192.168.1.100", "192.168.1.101"]
`,
			expectError: false,
		},
		{
			name: "delivery timeout changed (should succeed)",
			initialConfig: `
[inbound]
listen = ":8080"
[outbound]
source_ips = []
max_total_delivery_seconds = 30
`,
			updatedConfig: `
[inbound]
listen = ":8080"
[outbound]
source_ips = []
max_total_delivery_seconds = 60
`,
			expectError: false,
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
				} else if tt.errorContains != "" && !strings.Contains(err.Error(), tt.errorContains) {
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
[outbound]
source_ips_v4 = ["192.168.1.100"]
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
[outbound]
source_ips_v4 = ["192.168.1.100", "192.168.1.101"]
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
	} else if len(callbackConfig.Outbound.SourceIPsV4) != 2 {
		t.Errorf("expected 2 source IPv4s in callback, got %d", len(callbackConfig.Outbound.SourceIPsV4))
	}
}

func TestReloadableConfig_InvalidSyntax(t *testing.T) {
	logger := slog.Default()

	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.toml")

	validConfig := `
[inbound]
listen = ":8080"
[outbound]
source_ips = []
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
max_concurrent_requests = 100
[outbound]
source_ips = ["192.168.1.100"]
source_ip_selection = "round-robin"
max_total_delivery_seconds = 30
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
	if inboundCfg.MaxConcurrentRequests != 100 {
		t.Errorf("GetInbound: expected max_concurrent_requests 100, got %d", inboundCfg.MaxConcurrentRequests)
	}

	outboundCfg := rc.GetOutbound()
	if outboundCfg.SourceIPSelection != "round-robin" {
		t.Errorf("GetOutbound: expected source_ip_selection 'round-robin', got '%s'", outboundCfg.SourceIPSelection)
	}
	if outboundCfg.MaxTotalDeliverySeconds != 30 {
		t.Errorf("GetOutbound: expected max_total_delivery_seconds 30, got %d", outboundCfg.MaxTotalDeliverySeconds)
	}
}
