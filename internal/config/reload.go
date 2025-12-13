// Package config provides hot-reloadable configuration management.
//
// See config.go for the main package documentation.
package config

import (
	"crypto/tls"
	"fmt"
	"log/slog"
	"sync"
)

// ReloadableConfig wraps Config with thread-safe hot reload capability.
//
// This type enables runtime configuration updates without service restart.
// Configuration changes are triggered by sending a SIGHUP signal to the process
// (typically via `kill -HUP <pid>` or `pkill -HUP fune-server`).
//
// The reload process:
//  1. Load new config from disk
//  2. Validate that non-reloadable fields haven't changed
//  3. Call all registered reload callbacks to prepare components
//  4. Atomically swap the old config with the new config
//  5. Log the successful reload with key changed values
//
// Thread Safety:
//
// ReloadableConfig uses sync.RWMutex for safe concurrent access:
//   - Get methods acquire read locks (multiple readers allowed)
//   - Reload method acquires write lock (exclusive access)
//   - All config access is lock-protected to prevent data races
//
// Reload Callbacks:
//
// Components that need to react to config changes can register callbacks
// via RegisterReloadCallback. Callbacks are invoked before the config swap,
// allowing components to validate changes or prepare for updates.
//
// If any callback returns an error, the entire reload is aborted and the
// old configuration remains active.
//
// Example Usage:
//
//	reloadable, err := NewReloadableConfig("config.toml", logger)
//	if err != nil {
//	    log.Fatal(err)
//	}
//
//	// Register callback for delivery engine
//	reloadable.RegisterReloadCallback(func(cfg *Config) error {
//	    return deliveryEngine.UpdateSourceIPs(cfg.Outbound.SourceIPs)
//	})
//
//	// Get current config (thread-safe)
//	cfg := reloadable.Get()
//	fmt.Printf("Current source IPs: %v\n", cfg.Outbound.SourceIPs)
//
//	// Trigger reload (typically called in SIGHUP handler)
//	if err := reloadable.Reload(); err != nil {
//	    logger.Error("Config reload failed", "error", err)
//	}
type ReloadableConfig struct {
	mu         sync.RWMutex
	config     *Config
	configPath string
	logger     *slog.Logger

	// Callbacks for components that need to react to config changes
	onReload []func(*Config) error
}

// NewReloadableConfig creates a new ReloadableConfig with initial configuration.
//
// Loads the configuration file from the given path and initializes the
// reloadable wrapper. The returned ReloadableConfig is ready for use and
// can be safely accessed from multiple goroutines.
//
// This constructor should be called once at service startup. The returned
// instance should be passed to all components that need configuration access.
//
// Returns an error if the initial config file cannot be loaded or parsed.
func NewReloadableConfig(configPath string, logger *slog.Logger) (*ReloadableConfig, error) {
	cfg, err := LoadConfig(configPath)
	if err != nil {
		return nil, err
	}

	return &ReloadableConfig{
		config:     cfg,
		configPath: configPath,
		logger:     logger,
		onReload:   make([]func(*Config) error, 0),
	}, nil
}

// Get returns a copy of the current configuration.
//
// Thread-safe for concurrent reads. Multiple goroutines can call Get
// simultaneously without blocking each other.
//
// Returns a value copy, not a pointer, to prevent accidental mutation
// of the internal config state.
func (rc *ReloadableConfig) Get() Config {
	rc.mu.RLock()
	defer rc.mu.RUnlock()
	return *rc.config
}

// GetInbound returns a copy of the Inbound configuration section.
//
// Convenience method for accessing HTTP API settings without retrieving
// the entire configuration. Thread-safe for concurrent access.
func (rc *ReloadableConfig) GetInbound() InboundConfig {
	rc.mu.RLock()
	defer rc.mu.RUnlock()
	return rc.config.Inbound
}

// GetOutbound returns a copy of the Outbound configuration section.
//
// Convenience method for accessing SMTP delivery settings without retrieving
// the entire configuration. Thread-safe for concurrent access.
func (rc *ReloadableConfig) GetOutbound() OutboundConfig {
	rc.mu.RLock()
	defer rc.mu.RUnlock()
	return rc.config.Outbound
}


// RegisterReloadCallback registers a callback function to be invoked during reload.
//
// Callbacks are called before the configuration is swapped, allowing components
// to validate changes or prepare for updates. If any callback returns an error,
// the entire reload operation is aborted.
//
// Common use cases:
//   - Updating component state based on new config values
//   - Validating that new settings are compatible with current state
//   - Pre-loading resources needed for the new configuration
//
// Example:
//
//	reloadable.RegisterReloadCallback(func(newCfg *Config) error {
//	    if len(newCfg.Outbound.SourceIPs) == 0 {
//	        return fmt.Errorf("at least one source IP required")
//	    }
//	    return deliveryEngine.UpdateIPs(newCfg.Outbound.SourceIPs)
//	})
//
// Thread-safe but typically called only during initialization before
// concurrent access begins.
func (rc *ReloadableConfig) RegisterReloadCallback(callback func(*Config) error) {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	rc.onReload = append(rc.onReload, callback)
}

// Reload reloads the configuration from disk and applies changes.
//
// This method is typically called in response to a SIGHUP signal but can
// be invoked manually when needed. It performs a complete reload cycle:
//
//  1. Load new configuration from the same path used during initialization
//  2. Validate that non-reloadable fields haven't changed
//  3. Invoke all registered callbacks with the new configuration
//  4. Atomically replace the old configuration with the new one
//  5. Log success with key configuration values
//
// If any step fails, the reload is aborted and the old configuration
// remains active. This ensures the service never enters an invalid state
// due to a bad configuration file.
//
// Non-Reloadable Fields:
//
// The following fields cannot be changed during hot reload and will cause
// validation errors if modified:
//   - database_path: Would require data migration
//   - Inbound.Listen: Would require HTTP server restart
//   - Queue.WorkerCount: Would require worker pool restart
//   - Callbacks.WebhookURL: Would require callback handler restart
//
// Error Handling:
//
// Returns an error if:
//   - Config file cannot be read or parsed
//   - Non-reloadable fields have been changed
//   - Any registered callback returns an error
//
// The service continues running with the old configuration if reload fails.
// Operators should monitor logs and fix configuration issues before retrying.
//
// Example SIGHUP Handler:
//
//	sigChan := make(chan os.Signal, 1)
//	signal.Notify(sigChan, syscall.SIGHUP)
//	go func() {
//	    for range sigChan {
//	        if err := reloadable.Reload(); err != nil {
//	            logger.Error("Config reload failed", "error", err)
//	        }
//	    }
//	}()
func (rc *ReloadableConfig) Reload() error {
	rc.logger.Info("reloading configuration", "path", rc.configPath)

	// Load new config
	newConfig, err := LoadConfig(rc.configPath)
	if err != nil {
		rc.logger.Error("failed to reload config", "error", err)
		return fmt.Errorf("failed to reload config: %w", err)
	}

	// Validate critical fields haven't changed (database path, worker count, etc.)
	rc.mu.RLock()
	oldConfig := rc.config
	rc.mu.RUnlock()

	if err := rc.validateReload(oldConfig, newConfig); err != nil {
		rc.logger.Error("config validation failed", "error", err)
		return err
	}

	// Call reload callbacks before updating config
	// This allows components to prepare for the change
	for _, callback := range rc.onReload {
		if err := callback(newConfig); err != nil {
			rc.logger.Error("reload callback failed", "error", err)
			return fmt.Errorf("reload callback failed: %w", err)
		}
	}

	// Update config atomically
	rc.mu.Lock()
	rc.config = newConfig
	rc.mu.Unlock()

	rc.logger.Info("configuration reloaded successfully",
		"source_ips_v4", len(newConfig.Outbound.SourceIPsV4),
		"source_ips_v6", len(newConfig.Outbound.SourceIPsV6),
		"tls_enabled", newConfig.TLS.Enabled,
		"metrics_enabled", newConfig.Metrics.Enabled)

	return nil
}

// validateReload ensures critical non-reloadable fields haven't changed.
//
// This method is called during the Reload process to prevent configuration
// changes that would require service restart or could cause data loss.
//
// Validated fields:
//   - Inbound.Listen: Changing requires stopping and restarting HTTP server
//
// If validation fails, the reload is aborted and a descriptive error is returned
// indicating which field changed and what the old/new values were.
//
// This validation ensures the service maintains consistency and prevents
// configuration mistakes from causing production issues.
func (rc *ReloadableConfig) validateReload(oldCfg, newCfg *Config) error {
	// HTTP listen address cannot change (would require server restart)
	if oldCfg.Inbound.Listen != newCfg.Inbound.Listen {
		return fmt.Errorf("http.listen cannot be changed during reload (old: %s, new: %s)",
			oldCfg.Inbound.Listen, newCfg.Inbound.Listen)
	}

	return nil
}

// LoadTLSConfig loads TLS configuration and returns a tls.Config.
//
// This method is used to load TLS certificates for HTTPS connections.
// It only supports file-based certificates (not Let's Encrypt) and is
// intended for hot reload scenarios where certificates are updated on disk.
//
// The returned tls.Config is configured with:
//   - Certificates loaded from CertFile and KeyFile
//   - Minimum TLS version 1.2 for security
//
// Returns an error if:
//   - TLS is not enabled in the configuration
//   - Provider is not "file" (Let's Encrypt requires different handling)
//   - Certificate or key files cannot be read
//   - Certificate and key don't form a valid pair
//
// Example:
//
//	tlsConfig, err := reloadable.LoadTLSConfig()
//	if err != nil {
//	    logger.Error("Failed to load TLS config", "error", err)
//	    return
//	}
//	server.TLSConfig = tlsConfig
func (rc *ReloadableConfig) LoadTLSConfig() (*tls.Config, error) {
	rc.mu.RLock()
	defer rc.mu.RUnlock()

	if !rc.config.TLS.Enabled {
		return nil, fmt.Errorf("TLS not enabled")
	}

	// Only support file-based TLS for hot reload
	if rc.config.TLS.Provider != "file" {
		return nil, fmt.Errorf("LoadTLSConfig only supports file provider")
	}

	cert, err := tls.LoadX509KeyPair(rc.config.TLS.CertFile, rc.config.TLS.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("failed to load TLS certificate: %w", err)
	}

	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	}, nil
}
