// Package config provides TOML-based configuration management for the Fune SMTP delivery service.
//
// The configuration system supports:
//   - TOML file parsing with structured validation
//   - Hot reloading via SIGHUP signal for runtime updates
//   - Default value assignment for optional fields
//   - Type-safe access to configuration sections
//
// # Configuration Structure
//
// The configuration is divided into logical sections:
//   - Server: Core server settings (database path, PID file)
//   - Logging: Log level and format configuration
//   - Inbound: HTTP API settings (listen address, timeouts, rate limiting)
//   - TLS: TLS/SSL certificate configuration (file-based or Let's Encrypt)
//   - DNS: DNS resolver settings and caching
//   - Metrics: Prometheus metrics endpoint configuration
//   - Health: Health check endpoint settings
//   - Queue: Message queue and worker pool configuration
//   - Outbound: SMTP delivery settings (source IPs, timeouts, retry logic)
//   - Callbacks: Webhook notification configuration
//   - Reputation: IP reputation tracking and alerting
//   - Cluster: Gossip protocol clustering for multi-node deployments
//
// # Hot Reload Support
//
// Many configuration settings can be reloaded at runtime without service restart
// by sending a SIGHUP signal to the process. See ReloadableConfig in reload.go
// for the hot reload mechanism.
//
// Hot-reloadable settings include:
//   - Source IPs and IP selection strategy
//   - Rate limits (per-domain intervals)
//   - Circuit breaker thresholds
//   - DNS settings (resolvers, cache TTL)
//   - TLS certificates (file-based only)
//   - HTTP timeouts
//   - Retry delays and backoff multipliers
//   - Callback settings (except webhook URL)
//
// Non-reloadable settings (require restart):
//   - database_path: Changing requires data migration
//   - listen address: Requires HTTP server restart
//   - worker_count: Requires worker pool restart
//   - webhook_url: Requires callback handler restart
//
// # Usage Example
//
//	cfg, err := config.LoadConfig("config.toml")
//	if err != nil {
//	    log.Fatal(err)
//	}
//
//	// Access configuration sections
//	fmt.Printf("Database: %s\n", cfg.Server.DatabasePath)
//	fmt.Printf("Listen: %s\n", cfg.Inbound.Listen)
//	fmt.Printf("Source IPs: %v\n", cfg.Outbound.SourceIPs)
//
// # TOML Format
//
// Configuration files use TOML syntax with section headers:
//
//	[server]
//	database_path = "/var/lib/fune/queue.db"
//
//	[inbound]
//	listen = ":8025"
//	auth_token = "secret-token"
//
//	[outbound]
//	source_ips = ["192.168.1.100", "192.168.1.101"]
//	source_ip_selection = "round-robin"
//
// See config.toml.example in the repository root for a complete configuration reference.
package config

import (
	"fmt"
	"os"

	"github.com/BurntSushi/toml"
)

// Config is the root configuration structure containing all service settings.
// It maps directly to TOML sections in the configuration file.
//
// All fields use TOML tags for deserialization. Optional fields receive
// default values via SetDefaults().
type Config struct {
	Server     ServerConfig     `toml:"server"`
	Logging    LoggingConfig    `toml:"logging"`
	Inbound    InboundConfig    `toml:"inbound"`
	TLS        TLSConfig        `toml:"tls"`
	DNS        DNSConfig        `toml:"dns"`
	Metrics    MetricsConfig    `toml:"metrics"`
	Health     HealthConfig     `toml:"health"`
	Queue      QueueConfig      `toml:"queue"`
	Outbound   OutboundConfig   `toml:"outbound"`
	Callbacks  CallbacksConfig  `toml:"callbacks"`
	Reputation ReputationConfig `toml:"reputation"`
	Cluster    ClusterConfig    `toml:"cluster"`
}

// ServerConfig contains core server settings.
//
// These settings are not hot-reloadable and require a service restart to change.
type ServerConfig struct {
	DatabasePath string `toml:"database_path"` // Path to SQLite database file
	PIDFile      string `toml:"pid_file"`      // Path to PID file (default: fune.pid)
}

// LoggingConfig controls logging behavior for the service.
//
// Both fields are hot-reloadable.
type LoggingConfig struct {
	Level  string `toml:"level"`  // Log level: debug, info, warn, error (default: info)
	Format string `toml:"format"` // Log format: console, json (default: console)
}

// InboundConfig configures the HTTP API server that accepts message submissions.
//
// The HTTP API listens for POST requests to /v1/messages and queues messages
// for asynchronous delivery. It supports authentication via bearer tokens,
// rate limiting per IP address, and idempotency key handling for duplicate
// request detection.
//
// Hot-reloadable fields:
//   - AuthToken
//   - MaxBodySizeBytes
//   - ReadTimeoutSecs, WriteTimeoutSecs, IdleTimeoutSecs
//   - Rate limiting settings
//   - Idempotency settings
//
// Non-reloadable fields:
//   - Listen (requires HTTP server restart)
type InboundConfig struct {
	Listen           string `toml:"listen"`
	AuthToken        string `toml:"auth_token"`
	MaxBodySizeBytes int64  `toml:"max_body_size_bytes"`   // Maximum request body size (default: 35MB)
	ReadTimeoutSecs  int    `toml:"read_timeout_seconds"`  // Read timeout for requests (default: 30s)
	WriteTimeoutSecs int    `toml:"write_timeout_seconds"` // Write timeout for responses (default: 30s)
	IdleTimeoutSecs  int    `toml:"idle_timeout_seconds"`  // Idle timeout for keep-alive (default: 120s)

	// Rate limiting configuration
	RateLimitEnabled       bool `toml:"rate_limit_enabled"`         // Enable HTTP rate limiting (default: false)
	RateLimitRequestsPerIP int  `toml:"rate_limit_requests_per_ip"` // Max requests per IP per window (default: 100)
	RateLimitWindowSeconds int  `toml:"rate_limit_window_seconds"`  // Rate limit time window in seconds (default: 60)

	// Idempotency configuration
	IdempotencyEnabled  bool   `toml:"idempotency_enabled"`   // Enable idempotency key support (default: false)
	IdempotencyHeader   string `toml:"idempotency_header"`    // Header name for idempotency key (default: X-Idempotency-Key)
	IdempotencyTTLHours int    `toml:"idempotency_ttl_hours"` // How long to keep idempotency keys (default: 24)
}

// MetricsConfig configures the Prometheus metrics endpoint.
//
// The metrics endpoint exposes operational metrics including queue depth,
// delivery rates, circuit breaker state, and performance counters.
// Optional HTTP Basic Auth can protect the endpoint in production.
//
// All fields are hot-reloadable.
type MetricsConfig struct {
	Enabled  bool   `toml:"enabled"`  // Enable Prometheus metrics endpoint (default: true)
	Path     string `toml:"path"`     // Path for metrics endpoint (default: /metrics)
	Username string `toml:"username"` // HTTP Basic Auth username (optional, secure in production)
	Password string `toml:"password"` // HTTP Basic Auth password (optional, use strong password)
}

// HealthConfig configures the health check endpoint.
//
// The health endpoint provides liveness and readiness information about
// the service, including queue status, database connectivity, and circuit
// breaker state. Used by load balancers and orchestration systems.
//
// All fields are hot-reloadable except ListenAddr.
type HealthConfig struct {
	Enabled    bool   `toml:"enabled"`     // Enable health check endpoint (default: true)
	ListenAddr string `toml:"listen_addr"` // Address to listen on (default: :8080)
	Username   string `toml:"username"`    // HTTP Basic Auth username (optional)
	Password   string `toml:"password"`    // HTTP Basic Auth password (optional)
}

// TLSConfig configures TLS/SSL certificate handling for HTTPS.
//
// Supports two providers:
//   - "file": Load certificates from disk (supports hot reload via file monitoring)
//   - "letsencrypt": Automatic certificate provisioning via ACME protocol
//
// File-based certificates are hot-reloadable. Let's Encrypt certificates
// auto-renew but require S3 storage for multi-node cluster coordination.
type TLSConfig struct {
	Enabled     bool              `toml:"enabled"`
	CertFile    string            `toml:"cert_file"`
	KeyFile     string            `toml:"key_file"`
	Provider    string            `toml:"provider"` // "file" or "letsencrypt" (default: file)
	LetsEncrypt LetsEncryptConfig `toml:"letsencrypt"`
}

// LetsEncryptConfig configures automatic certificate provisioning via Let's Encrypt.
//
// Requires S3 storage for certificate persistence and cluster-wide sharing.
// The email address receives expiration notices and important updates from
// Let's Encrypt.
type LetsEncryptConfig struct {
	Email           string   `toml:"email"`
	Domains         []string `toml:"domains"`
	StorageProvider string   `toml:"storage_provider"` // "s3" (default: s3)
	S3              S3Config `toml:"s3"`
}

// S3Config provides AWS S3 credentials for Let's Encrypt certificate storage.
//
// Enables multi-node clusters to share certificates and coordinate renewals.
type S3Config struct {
	Bucket          string `toml:"bucket"`
	Region          string `toml:"region"`
	AccessKeyID     string `toml:"access_key_id"`
	SecretAccessKey string `toml:"secret_access_key"`
}

// QueueConfig configures the message queue and worker pool behavior.
//
// The worker pool processes queued messages in batches. Workers receive
// instant notifications via Go channels when new messages arrive, with
// PollIntervalSeconds as a fallback polling mechanism.
//
// Hot-reloadable fields:
//   - BatchSize
//   - CleanupIntervalSeconds
//   - PollIntervalSeconds
//
// Non-reloadable fields:
//   - WorkerCount (requires worker pool restart)
type QueueConfig struct {
	WorkerCount            int `toml:"worker_count"`             // Number of concurrent workers (default: 10)
	BatchSize              int `toml:"batch_size"`               // Messages to process per batch (default: 5)
	CleanupIntervalSeconds int `toml:"cleanup_interval_seconds"` // Cleanup expired messages interval (default: 60s)
	PollIntervalSeconds    int `toml:"poll_interval_seconds"`    // Fallback poll interval when no notifications (default: 30s)
}

// DNSConfig configures DNS resolution and caching behavior.
//
// Supports custom DNS resolvers with round-robin load balancing and
// UDP to TCP fallback. Implements two-tier caching: positive cache for
// successful lookups and negative cache for NXDOMAIN responses.
//
// All fields are hot-reloadable.
type DNSConfig struct {
	Resolvers               []string `toml:"resolvers"`                  // Custom DNS servers (empty = system default)
	TimeoutSeconds          int      `toml:"timeout_seconds"`            // Timeout for DNS queries (default: 5s)
	CacheTTLSeconds         int      `toml:"cache_ttl_seconds"`          // Cache successful DNS lookups (default: 3600s)
	CacheNegativeTTLSeconds int      `toml:"cache_negative_ttl_seconds"` // Cache failed lookups/NXDOMAIN (default: 60s)
}

// OutboundConfig configures SMTP delivery behavior and settings.
//
// Controls how messages are delivered to recipient MX servers, including:
//   - Source IP selection and rotation strategies
//   - Connection and SMTP protocol timeouts
//   - Retry logic with exponential backoff
//   - Per-domain rate limiting to prevent overwhelming recipients
//   - Circuit breaker for delivery failure protection
//   - IPv6-first delivery with IPv4 fallback
//
// The delivery engine uses intelligent retry scheduling:
//   - Temporary failures: Exponential backoff (5min -> 10min -> 20min -> ... -> 12hr)
//   - Greylisting (421): Fast retry after 2 minutes
//   - Permanent failures (5xx): No retry, immediate hard bounce
//   - Max age: Messages expire after 48 hours (configurable)
//
// All fields are hot-reloadable.
type OutboundConfig struct {
	SourceIPs                 []string `toml:"source_ips"`
	SourceIPSelection         string   `toml:"source_ip_selection"`          // "round-robin", "random", "hash-domain" (default: round-robin)
	MXCacheTTLSeconds         int      `toml:"mx_cache_ttl_seconds"`         // MX record cache TTL (default: 3600s)
	ConnectionTimeoutSeconds  int      `toml:"connection_timeout_seconds"`   // TCP connection timeout (default: 30s)
	SMTPTimeoutSeconds        int      `toml:"smtp_timeout_seconds"`         // SMTP command timeout (default: 60s)
	MaxMessageAgeHours        int      `toml:"max_message_age_hours"`        // Max age before expiry (default: 48h)
	InitialRetryDelaySeconds  int      `toml:"initial_retry_delay_seconds"`  // First retry delay (default: 300s)
	MaxRetryDelaySeconds      int      `toml:"max_retry_delay_seconds"`      // Maximum retry delay cap (default: 43200s/12h)
	BackoffMultiplier         float64  `toml:"backoff_multiplier"`           // Exponential backoff multiplier (default: 2.0)
	GreylistRetryDelaySeconds int      `toml:"greylist_retry_delay_seconds"` // Fast retry for 421 greylisting (default: 120s)
	MaxIPsPerMX               int      `toml:"max_ips_per_mx"`               // Maximum number of IPs to try per MX host (default: 5)

	// Rate limiting per destination domain
	PerDomainIntervalSeconds int `toml:"per_domain_interval_seconds"` // Minimum seconds between deliveries to same domain (default: 2s)
	PerDomainRetrySeconds    int `toml:"per_domain_retry_seconds"`    // Delay before retrying throttled message (default: 5s)

	// Circuit breaker configuration
	CircuitBreakerEnabled          bool `toml:"circuit_breaker_enabled"`              // Enable circuit breaker (default: true)
	CircuitBreakerFailureThreshold int  `toml:"circuit_breaker_failure_threshold"`    // Consecutive failures to open circuit (default: 5)
	CircuitBreakerSuccessThreshold int  `toml:"circuit_breaker_success_threshold"`    // Consecutive successes to close circuit (default: 2)
	CircuitBreakerOpenTimeoutSecs  int  `toml:"circuit_breaker_open_timeout_seconds"` // Timeout before half-open retry (default: 60s)
}

// CallbacksConfig configures webhook notifications for delivery events.
//
// After each delivery attempt (success, bounce, or defer), the service sends
// an HTTP POST to the configured webhook URL with delivery details. Callbacks
// are queued separately from messages and have their own retry logic and
// circuit breaker protection.
//
// Callback events include:
//   - Delivered: Successful delivery to MX server
//   - Bounced: Permanent failure (5xx SMTP error)
//   - Deferred: Temporary failure with retry scheduled
//   - Expired: Message exceeded max age without delivery
//
// Hot-reloadable fields: All except WebhookURL
//
// Non-reloadable fields:
//   - WebhookURL (requires callback handler restart)
type CallbacksConfig struct {
	WebhookURL               string  `toml:"webhook_url"`
	AuthToken                string  `toml:"auth_token"`
	TimeoutSeconds           int     `toml:"timeout_seconds"`             // HTTP request timeout (default: 10s)
	MaxCallbackAgeHours      int     `toml:"max_callback_age_hours"`      // Max age before expiry (default: 48h)
	InitialRetryDelaySeconds int     `toml:"initial_retry_delay_seconds"` // First retry delay (default: 30s)
	MaxRetryDelaySeconds     int     `toml:"max_retry_delay_seconds"`     // Maximum retry delay cap (default: 3600s/1h)
	BackoffMultiplier        float64 `toml:"backoff_multiplier"`          // Exponential backoff multiplier (default: 2.0)
	BatchSize                int     `toml:"batch_size"`                  // Callbacks to process per batch (default: 10)

	// Circuit breaker configuration
	CircuitBreakerEnabled          bool `toml:"circuit_breaker_enabled"`              // Enable circuit breaker (default: true)
	CircuitBreakerFailureThreshold int  `toml:"circuit_breaker_failure_threshold"`    // Consecutive failures to open circuit (default: 5)
	CircuitBreakerSuccessThreshold int  `toml:"circuit_breaker_success_threshold"`    // Consecutive successes to close circuit (default: 2)
	CircuitBreakerOpenTimeoutSecs  int  `toml:"circuit_breaker_open_timeout_seconds"` // Timeout before half-open retry (default: 60s)
}

// ReputationConfig configures IP reputation tracking and alerting.
//
// Monitors SMTP responses for reputation-related errors (550/554 with keywords
// like "blacklist", "reputation", "blocked"). Degraded IPs are automatically
// removed from the rotation pool and webhook alerts are sent to notify operators.
//
// After the configured retry period, degraded IPs are automatically retried.
// If they continue to fail, they remain degraded. Successful deliveries restore
// the IP to good standing.
//
// All fields are hot-reloadable.
type ReputationConfig struct {
	AlertWebhookURL        string `toml:"alert_webhook_url"`         // Webhook URL for reputation alerts
	AlertAuthToken         string `toml:"alert_auth_token"`          // Bearer token for alert webhook
	AlertTimeoutSeconds    int    `toml:"alert_timeout_seconds"`     // Alert webhook timeout (default: 10s)
	DegradedRetryHours     int    `toml:"degraded_retry_hours"`      // Hours before retrying degraded IP (default: 48h)
	EnableIPTracking       bool   `toml:"enable_ip_tracking"`        // Enable IP reputation tracking (default: false)
	DegradedIPCleanupHours int    `toml:"degraded_ip_cleanup_hours"` // Hours before cleaning up old degraded IPs (default: 168h/7d)
}

// ClusterConfig configures the gossip protocol for multi-node clustering.
//
// Uses HashiCorp memberlist for cluster membership, leader election, and
// distributed state sharing. Enables features like:
//   - Let's Encrypt certificate coordination (leader handles ACME challenges)
//   - Distributed idempotency key checking across nodes
//   - Cluster-wide health monitoring
//
// Requires a secret key for AES-256 encryption of cluster communication.
// The secret key must be a 32-byte base64-encoded string shared by all nodes.
//
// All fields are hot-reloadable except BindAddr and BindPort.
type ClusterConfig struct {
	Enabled   bool     `toml:"enabled"`
	BindAddr  string   `toml:"bind_addr"`  // IP address to bind to (default: 0.0.0.0)
	BindPort  int      `toml:"bind_port"`  // Gossip protocol port (default: 7946)
	Peers     []string `toml:"peers"`      // All other cluster nodes (address:port)
	NodeID    string   `toml:"node_id"`    // Unique node identifier (hostname recommended)
	SecretKey string   `toml:"secret_key"` // 32-byte base64 encoded encryption key for AES-256
}

// SetDefaults sets default values for optional configuration fields.
//
// This method is called automatically by LoadConfig after parsing the TOML file.
// It assigns sensible production defaults to any fields not explicitly set in
// the configuration file.
//
// Default values are chosen to balance safety and performance:
//   - Conservative timeouts to prevent hanging operations
//   - Circuit breakers enabled by default for production safety
//   - Moderate batch sizes for throughput without resource exhaustion
//   - Reasonable cache TTLs to balance freshness and load
//
// Some defaults that might need adjustment for high-volume deployments:
//   - WorkerCount: 10 workers (increase for higher throughput)
//   - BatchSize: 5 messages per batch (tune based on load)
//   - SourceIPSelection: round-robin (consider hash-domain for consistency)
func (c *Config) SetDefaults() {
	if c.Server.PIDFile == "" {
		c.Server.PIDFile = "fune.pid"
	}
	if c.Logging.Level == "" {
		c.Logging.Level = "info"
	}
	if c.Logging.Format == "" {
		c.Logging.Format = "console"
	}
	if c.Inbound.MaxBodySizeBytes == 0 {
		c.Inbound.MaxBodySizeBytes = 35 * 1024 * 1024
	}
	if c.Inbound.ReadTimeoutSecs == 0 {
		c.Inbound.ReadTimeoutSecs = 30
	}
	if c.Inbound.WriteTimeoutSecs == 0 {
		c.Inbound.WriteTimeoutSecs = 30
	}
	if c.Inbound.IdleTimeoutSecs == 0 {
		c.Inbound.IdleTimeoutSecs = 120
	}
	if c.Inbound.RateLimitRequestsPerIP == 0 {
		c.Inbound.RateLimitRequestsPerIP = 100
	}
	if c.Inbound.RateLimitWindowSeconds == 0 {
		c.Inbound.RateLimitWindowSeconds = 60
	}
	if c.Inbound.IdempotencyHeader == "" {
		c.Inbound.IdempotencyHeader = "X-Idempotency-Key"
	}
	if c.Inbound.IdempotencyTTLHours == 0 {
		c.Inbound.IdempotencyTTLHours = 24
	}

	// Metrics defaults
	if c.Metrics.Path == "" {
		c.Metrics.Path = "/metrics"
	}

	// Health defaults
	if c.Health.ListenAddr == "" {
		c.Health.ListenAddr = ":8080"
	}

	if c.TLS.Provider == "" {
		c.TLS.Provider = "file"
	}
	if c.TLS.LetsEncrypt.StorageProvider == "" {
		c.TLS.LetsEncrypt.StorageProvider = "s3"
	}
	if c.Queue.WorkerCount == 0 {
		c.Queue.WorkerCount = 10
	}
	if c.Queue.BatchSize == 0 {
		c.Queue.BatchSize = 5
	}
	if c.Queue.CleanupIntervalSeconds == 0 {
		c.Queue.CleanupIntervalSeconds = 60
	}
	if c.Queue.PollIntervalSeconds == 0 {
		c.Queue.PollIntervalSeconds = 30
	}
	if c.Outbound.MXCacheTTLSeconds == 0 {
		c.Outbound.MXCacheTTLSeconds = 3600
	}
	if c.Outbound.ConnectionTimeoutSeconds == 0 {
		c.Outbound.ConnectionTimeoutSeconds = 30
	}
	if c.Outbound.SMTPTimeoutSeconds == 0 {
		c.Outbound.SMTPTimeoutSeconds = 60
	}
	if c.Outbound.MaxMessageAgeHours == 0 {
		c.Outbound.MaxMessageAgeHours = 48
	}
	if c.Outbound.InitialRetryDelaySeconds == 0 {
		c.Outbound.InitialRetryDelaySeconds = 300
	}
	if c.Outbound.MaxRetryDelaySeconds == 0 {
		c.Outbound.MaxRetryDelaySeconds = 43200
	}
	if c.Outbound.BackoffMultiplier == 0 {
		c.Outbound.BackoffMultiplier = 2.0
	}
	if c.Outbound.GreylistRetryDelaySeconds == 0 {
		c.Outbound.GreylistRetryDelaySeconds = 120
	}
	if c.Outbound.SourceIPSelection == "" {
		c.Outbound.SourceIPSelection = "round-robin"
	}
	if c.Outbound.MaxIPsPerMX == 0 {
		c.Outbound.MaxIPsPerMX = 5
	}
	if c.Outbound.PerDomainIntervalSeconds == 0 {
		c.Outbound.PerDomainIntervalSeconds = 2
	}
	if c.Outbound.PerDomainRetrySeconds == 0 {
		c.Outbound.PerDomainRetrySeconds = 5
	}
	// Circuit breaker enabled by default for production safety
	// Note: Boolean fields default to false in Go, so we check if explicitly disabled
	// If not explicitly set in config, enable by default
	c.Outbound.CircuitBreakerEnabled = true
	if c.Outbound.CircuitBreakerFailureThreshold == 0 {
		c.Outbound.CircuitBreakerFailureThreshold = 5
	}
	if c.Outbound.CircuitBreakerSuccessThreshold == 0 {
		c.Outbound.CircuitBreakerSuccessThreshold = 2
	}
	if c.Outbound.CircuitBreakerOpenTimeoutSecs == 0 {
		c.Outbound.CircuitBreakerOpenTimeoutSecs = 60
	}
	// DNS defaults
	if c.DNS.TimeoutSeconds == 0 {
		c.DNS.TimeoutSeconds = 5
	}
	if c.DNS.CacheTTLSeconds == 0 {
		c.DNS.CacheTTLSeconds = 3600
	}
	if c.DNS.CacheNegativeTTLSeconds == 0 {
		c.DNS.CacheNegativeTTLSeconds = 60
	}
	if c.Callbacks.TimeoutSeconds == 0 {
		c.Callbacks.TimeoutSeconds = 10
	}
	if c.Callbacks.MaxCallbackAgeHours == 0 {
		c.Callbacks.MaxCallbackAgeHours = 48
	}
	if c.Callbacks.InitialRetryDelaySeconds == 0 {
		c.Callbacks.InitialRetryDelaySeconds = 30
	}
	if c.Callbacks.MaxRetryDelaySeconds == 0 {
		c.Callbacks.MaxRetryDelaySeconds = 3600
	}
	if c.Callbacks.BackoffMultiplier == 0 {
		c.Callbacks.BackoffMultiplier = 2.0
	}
	if c.Callbacks.BatchSize == 0 {
		c.Callbacks.BatchSize = 10
	}
	// Callback circuit breaker enabled by default for production safety
	c.Callbacks.CircuitBreakerEnabled = true
	if c.Callbacks.CircuitBreakerFailureThreshold == 0 {
		c.Callbacks.CircuitBreakerFailureThreshold = 5
	}
	if c.Callbacks.CircuitBreakerSuccessThreshold == 0 {
		c.Callbacks.CircuitBreakerSuccessThreshold = 2
	}
	if c.Callbacks.CircuitBreakerOpenTimeoutSecs == 0 {
		c.Callbacks.CircuitBreakerOpenTimeoutSecs = 60
	}
	if c.Reputation.AlertTimeoutSeconds == 0 {
		c.Reputation.AlertTimeoutSeconds = 10
	}
	if c.Reputation.DegradedRetryHours == 0 {
		c.Reputation.DegradedRetryHours = 48
	}
	if c.Reputation.DegradedIPCleanupHours == 0 {
		c.Reputation.DegradedIPCleanupHours = 168
	}
	if c.Cluster.BindAddr == "" {
		c.Cluster.BindAddr = "0.0.0.0"
	}
	if c.Cluster.BindPort == 0 {
		c.Cluster.BindPort = 7946
	}
}

// LoadConfig loads and parses a TOML configuration file from the given path.
//
// The function performs the following steps:
//  1. Reads the configuration file from disk
//  2. Parses TOML content into Config struct
//  3. Applies default values via SetDefaults()
//  4. Returns the fully initialized configuration
//
// The returned Config is ready for immediate use. No validation is performed
// at load time; validation occurs during hot reload via ReloadableConfig.
//
// Example:
//
//	cfg, err := LoadConfig("config.toml")
//	if err != nil {
//	    log.Fatalf("Failed to load config: %v", err)
//	}
//
//	fmt.Printf("Listening on: %s\n", cfg.Inbound.Listen)
//
// Returns an error if:
//   - File cannot be read (missing, permission denied, etc.)
//   - TOML syntax is invalid
//   - TOML structure doesn't match Config struct
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}

	var config Config
	if err := toml.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("failed to parse config: %w", err)
	}

	config.SetDefaults()

	return &config, nil
}
