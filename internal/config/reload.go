package config

import (
	"crypto/tls"
	"fmt"
	"sync"

	"go.uber.org/zap"
)

// ReloadableConfig wraps Config with thread-safe reload capability
type ReloadableConfig struct {
	mu         sync.RWMutex
	config     *Config
	configPath string
	logger     *zap.Logger

	// Callbacks for components that need to react to config changes
	onReload []func(*Config) error
}

// NewReloadableConfig creates a new reloadable config
func NewReloadableConfig(configPath string, logger *zap.Logger) (*ReloadableConfig, error) {
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

// Get returns a copy of the current config (thread-safe read)
func (rc *ReloadableConfig) Get() Config {
	rc.mu.RLock()
	defer rc.mu.RUnlock()
	return *rc.config
}

// GetHTTP returns a copy of HTTP config
func (rc *ReloadableConfig) GetHTTP() HTTPConfig {
	rc.mu.RLock()
	defer rc.mu.RUnlock()
	return rc.config.HTTP
}

// GetDelivery returns a copy of Delivery config
func (rc *ReloadableConfig) GetDelivery() DeliveryConfig {
	rc.mu.RLock()
	defer rc.mu.RUnlock()
	return rc.config.Delivery
}

// GetQueue returns a copy of Queue config
func (rc *ReloadableConfig) GetQueue() QueueConfig {
	rc.mu.RLock()
	defer rc.mu.RUnlock()
	return rc.config.Queue
}

// GetCallbacks returns a copy of Callbacks config
func (rc *ReloadableConfig) GetCallbacks() CallbacksConfig {
	rc.mu.RLock()
	defer rc.mu.RUnlock()
	return rc.config.Callbacks
}

// RegisterReloadCallback registers a callback to be called when config is reloaded
func (rc *ReloadableConfig) RegisterReloadCallback(callback func(*Config) error) {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	rc.onReload = append(rc.onReload, callback)
}

// Reload reloads the configuration from disk
func (rc *ReloadableConfig) Reload() error {
	rc.logger.Info("reloading configuration", zap.String("path", rc.configPath))

	// Load new config
	newConfig, err := LoadConfig(rc.configPath)
	if err != nil {
		rc.logger.Error("failed to reload config", zap.Error(err))
		return fmt.Errorf("failed to reload config: %w", err)
	}

	// Validate critical fields haven't changed (database path, worker count, etc.)
	rc.mu.RLock()
	oldConfig := rc.config
	rc.mu.RUnlock()

	if err := rc.validateReload(oldConfig, newConfig); err != nil {
		rc.logger.Error("config validation failed", zap.Error(err))
		return err
	}

	// Call reload callbacks before updating config
	// This allows components to prepare for the change
	for _, callback := range rc.onReload {
		if err := callback(newConfig); err != nil {
			rc.logger.Error("reload callback failed", zap.Error(err))
			return fmt.Errorf("reload callback failed: %w", err)
		}
	}

	// Update config atomically
	rc.mu.Lock()
	rc.config = newConfig
	rc.mu.Unlock()

	rc.logger.Info("configuration reloaded successfully",
		zap.Int("source_ips", len(newConfig.Delivery.SourceIPs)),
		zap.Bool("tls_enabled", newConfig.TLS.Enabled),
		zap.Bool("metrics_enabled", newConfig.HTTP.MetricsEnabled),
		zap.Bool("circuit_breaker_enabled", newConfig.Delivery.CircuitBreakerEnabled))

	return nil
}

// validateReload ensures critical fields haven't changed
func (rc *ReloadableConfig) validateReload(oldCfg, newCfg *Config) error {
	// Database path cannot change (would require migration)
	if oldCfg.Server.DatabasePath != newCfg.Server.DatabasePath {
		return fmt.Errorf("database_path cannot be changed during reload (old: %s, new: %s)",
			oldCfg.Server.DatabasePath, newCfg.Server.DatabasePath)
	}

	// HTTP listen address cannot change (would require server restart)
	if oldCfg.HTTP.Listen != newCfg.HTTP.Listen {
		return fmt.Errorf("http.listen cannot be changed during reload (old: %s, new: %s)",
			oldCfg.HTTP.Listen, newCfg.HTTP.Listen)
	}

	// Worker count cannot change (would require worker pool restart)
	if oldCfg.Queue.WorkerCount != newCfg.Queue.WorkerCount {
		return fmt.Errorf("worker_count cannot be changed during reload (old: %d, new: %d)",
			oldCfg.Queue.WorkerCount, newCfg.Queue.WorkerCount)
	}

	// Webhook URL cannot change (would require callback handler restart)
	if oldCfg.Callbacks.WebhookURL != newCfg.Callbacks.WebhookURL {
		return fmt.Errorf("webhook_url cannot be changed during reload (old: %s, new: %s)",
			oldCfg.Callbacks.WebhookURL, newCfg.Callbacks.WebhookURL)
	}

	return nil
}

// LoadTLSConfig loads TLS configuration and returns tls.Config
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
