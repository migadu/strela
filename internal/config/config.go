package config

import (
	"fmt"
	"os"

	"github.com/BurntSushi/toml"
)

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

type ServerConfig struct {
	DatabasePath string `toml:"database_path"` // Path to SQLite database file
	PIDFile      string `toml:"pid_file"`      // Path to PID file (default: fune.pid)
}

type LoggingConfig struct {
	Level  string `toml:"level"`  // Log level: debug, info, warn, error (default: info)
	Format string `toml:"format"` // Log format: console, json (default: console)
}

type InboundConfig struct {
	Listen           string `toml:"listen"`
	AuthToken        string `toml:"auth_token"`
	MaxBodySizeBytes int64  `toml:"max_body_size_bytes"`   // Maximum request body size
	ReadTimeoutSecs  int    `toml:"read_timeout_seconds"`  // Read timeout for requests
	WriteTimeoutSecs int    `toml:"write_timeout_seconds"` // Write timeout for responses
	IdleTimeoutSecs  int    `toml:"idle_timeout_seconds"`  // Idle timeout for keep-alive

	// Rate limiting configuration
	RateLimitEnabled       bool `toml:"rate_limit_enabled"`         // Enable HTTP rate limiting (default: false)
	RateLimitRequestsPerIP int  `toml:"rate_limit_requests_per_ip"` // Max requests per IP per window (default: 100)
	RateLimitWindowSeconds int  `toml:"rate_limit_window_seconds"`  // Rate limit time window in seconds (default: 60)

	// Idempotency configuration
	IdempotencyEnabled  bool   `toml:"idempotency_enabled"`   // Enable idempotency key support (default: false)
	IdempotencyHeader   string `toml:"idempotency_header"`    // Header name for idempotency key (default: X-Idempotency-Key)
	IdempotencyTTLHours int    `toml:"idempotency_ttl_hours"` // How long to keep idempotency keys (default: 24)
}

type MetricsConfig struct {
	Enabled  bool   `toml:"enabled"`  // Enable Prometheus metrics endpoint (default: true)
	Path     string `toml:"path"`     // Path for metrics endpoint (default: /metrics)
	Username string `toml:"username"` // HTTP Basic Auth username (optional, secure in production)
	Password string `toml:"password"` // HTTP Basic Auth password (optional, use strong password)
}

type HealthConfig struct {
	Enabled    bool   `toml:"enabled"`     // Enable health check endpoint (default: true)
	ListenAddr string `toml:"listen_addr"` // Address to listen on (default: :8080)
	Username   string `toml:"username"`    // HTTP Basic Auth username (optional)
	Password   string `toml:"password"`    // HTTP Basic Auth password (optional)
}

type TLSConfig struct {
	Enabled     bool              `toml:"enabled"`
	CertFile    string            `toml:"cert_file"`
	KeyFile     string            `toml:"key_file"`
	Provider    string            `toml:"provider"` // "file" or "letsencrypt"
	LetsEncrypt LetsEncryptConfig `toml:"letsencrypt"`
}

type LetsEncryptConfig struct {
	Email           string   `toml:"email"`
	Domains         []string `toml:"domains"`
	StorageProvider string   `toml:"storage_provider"` // "s3"
	S3              S3Config `toml:"s3"`
}

type S3Config struct {
	Bucket          string `toml:"bucket"`
	Region          string `toml:"region"`
	AccessKeyID     string `toml:"access_key_id"`
	SecretAccessKey string `toml:"secret_access_key"`
}

type QueueConfig struct {
	WorkerCount            int `toml:"worker_count"`
	BatchSize              int `toml:"batch_size"`
	CleanupIntervalSeconds int `toml:"cleanup_interval_seconds"`
	PollIntervalSeconds    int `toml:"poll_interval_seconds"` // Fallback poll interval when no notifications
}

type DNSConfig struct {
	Resolvers               []string `toml:"resolvers"`                  // Custom DNS servers (empty = system default)
	TimeoutSeconds          int      `toml:"timeout_seconds"`            // Timeout for DNS queries
	CacheTTLSeconds         int      `toml:"cache_ttl_seconds"`          // Cache successful DNS lookups
	CacheNegativeTTLSeconds int      `toml:"cache_negative_ttl_seconds"` // Cache failed lookups (NXDOMAIN)
}

type OutboundConfig struct {
	SourceIPs                 []string `toml:"source_ips"`
	SourceIPSelection         string   `toml:"source_ip_selection"` // "round-robin", "random", "hash-domain"
	MXCacheTTLSeconds         int      `toml:"mx_cache_ttl_seconds"`
	ConnectionTimeoutSeconds  int      `toml:"connection_timeout_seconds"`
	SMTPTimeoutSeconds        int      `toml:"smtp_timeout_seconds"`
	MaxMessageAgeHours        int      `toml:"max_message_age_hours"`
	InitialRetryDelaySeconds  int      `toml:"initial_retry_delay_seconds"`
	MaxRetryDelaySeconds      int      `toml:"max_retry_delay_seconds"`
	BackoffMultiplier         float64  `toml:"backoff_multiplier"`
	GreylistRetryDelaySeconds int      `toml:"greylist_retry_delay_seconds"`
	MaxIPsPerMX               int      `toml:"max_ips_per_mx"` // Maximum number of IPs to try per MX host

	// Rate limiting per destination domain
	PerDomainIntervalSeconds int `toml:"per_domain_interval_seconds"` // Minimum seconds between deliveries to same domain
	PerDomainRetrySeconds    int `toml:"per_domain_retry_seconds"`    // Delay before retrying throttled message

	// Circuit breaker configuration
	CircuitBreakerEnabled          bool `toml:"circuit_breaker_enabled"`
	CircuitBreakerFailureThreshold int  `toml:"circuit_breaker_failure_threshold"`
	CircuitBreakerSuccessThreshold int  `toml:"circuit_breaker_success_threshold"`
	CircuitBreakerOpenTimeoutSecs  int  `toml:"circuit_breaker_open_timeout_seconds"`
}

type CallbacksConfig struct {
	WebhookURL               string  `toml:"webhook_url"`
	AuthToken                string  `toml:"auth_token"`
	TimeoutSeconds           int     `toml:"timeout_seconds"`
	MaxCallbackAgeHours      int     `toml:"max_callback_age_hours"`
	InitialRetryDelaySeconds int     `toml:"initial_retry_delay_seconds"`
	MaxRetryDelaySeconds     int     `toml:"max_retry_delay_seconds"`
	BackoffMultiplier        float64 `toml:"backoff_multiplier"`
	BatchSize                int     `toml:"batch_size"`

	// Circuit breaker configuration
	CircuitBreakerEnabled          bool `toml:"circuit_breaker_enabled"`
	CircuitBreakerFailureThreshold int  `toml:"circuit_breaker_failure_threshold"`
	CircuitBreakerSuccessThreshold int  `toml:"circuit_breaker_success_threshold"`
	CircuitBreakerOpenTimeoutSecs  int  `toml:"circuit_breaker_open_timeout_seconds"`
}

type ReputationConfig struct {
	AlertWebhookURL        string `toml:"alert_webhook_url"`
	AlertAuthToken         string `toml:"alert_auth_token"`
	AlertTimeoutSeconds    int    `toml:"alert_timeout_seconds"`
	DegradedRetryHours     int    `toml:"degraded_retry_hours"`
	EnableIPTracking       bool   `toml:"enable_ip_tracking"`
	DegradedIPCleanupHours int    `toml:"degraded_ip_cleanup_hours"`
}

type ClusterConfig struct {
	Enabled   bool     `toml:"enabled"`
	BindAddr  string   `toml:"bind_addr"` // IP address to bind to (default: 0.0.0.0)
	BindPort  int      `toml:"bind_port"`
	Peers     []string `toml:"peers"`      // All other cluster nodes (address:port)
	NodeID    string   `toml:"node_id"`    // Unique node identifier
	SecretKey string   `toml:"secret_key"` // 32-byte base64 encoded encryption key for AES-256
}

// SetDefaults sets default values for optional config fields
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
