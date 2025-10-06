package config

import (
	"fmt"
	"os"

	"github.com/BurntSushi/toml"
)

type Config struct {
	Server     ServerConfig     `toml:"server"`
	Logging    LoggingConfig    `toml:"logging"`
	HTTP       HTTPConfig       `toml:"http"`
	Queue      QueueConfig      `toml:"queue"`
	Delivery   DeliveryConfig   `toml:"delivery"`
	Callbacks  CallbacksConfig  `toml:"callbacks"`
	Reputation ReputationConfig `toml:"reputation"`
	Gossip     GossipConfig     `toml:"gossip"`
}

type ServerConfig struct {
	DatabasePath string `toml:"database_path"` // Path to SQLite database file
	PIDFile      string `toml:"pid_file"`      // Path to PID file (default: fune.pid)
}

type LoggingConfig struct {
	Level  string `toml:"level"`  // Log level: debug, info, warn, error (default: info)
	Format string `toml:"format"` // Log format: console, json (default: console)
	// Note: Logs always go to stdout/stderr for systemd compatibility
	// Use systemd's journald for log rotation and management in production
}

type HTTPConfig struct {
	Listen           string `toml:"listen"`
	AuthToken        string `toml:"auth_token"`
	MaxBodySizeBytes int64  `toml:"max_body_size_bytes"`   // Maximum request body size
	ReadTimeoutSecs  int    `toml:"read_timeout_seconds"`  // Read timeout for requests
	WriteTimeoutSecs int    `toml:"write_timeout_seconds"` // Write timeout for responses
	IdleTimeoutSecs  int    `toml:"idle_timeout_seconds"`  // Idle timeout for keep-alive

	// TLS configuration
	TLSEnabled  bool   `toml:"tls_enabled"`   // Enable HTTPS with TLS
	TLSCertFile string `toml:"tls_cert_file"` // Path to TLS certificate file
	TLSKeyFile  string `toml:"tls_key_file"`  // Path to TLS private key file

	// Metrics configuration
	MetricsEnabled bool   `toml:"metrics_enabled"` // Enable Prometheus metrics endpoint (default: true)
	MetricsPath    string `toml:"metrics_path"`    // Path for metrics endpoint (default: /metrics)

	// Idempotency configuration
	IdempotencyEnabled  bool   `toml:"idempotency_enabled"`   // Enable idempotency key support (default: false)
	IdempotencyHeader   string `toml:"idempotency_header"`    // Header name for idempotency key (default: X-Idempotency-Key)
	IdempotencyTTLHours int    `toml:"idempotency_ttl_hours"` // How long to keep idempotency keys (default: 24)
}

type QueueConfig struct {
	WorkerCount            int `toml:"worker_count"`
	BatchSize              int `toml:"batch_size"`
	CleanupIntervalSeconds int `toml:"cleanup_interval_seconds"`
	PollIntervalSeconds    int `toml:"poll_interval_seconds"` // Fallback poll interval when no notifications
}

type DeliveryConfig struct {
	SourceIPs                 []string `toml:"source_ips"`
	IPSelection               string   `toml:"ip_selection"` // "round-robin", "random", "hash-domain"
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
	CircuitBreakerEnabled          bool `toml:"circuit_breaker_enabled"`              // Enable circuit breaker (default: true)
	CircuitBreakerFailureThreshold int  `toml:"circuit_breaker_failure_threshold"`    // Consecutive failures before opening (default: 5)
	CircuitBreakerSuccessThreshold int  `toml:"circuit_breaker_success_threshold"`    // Successes in half-open to close (default: 2)
	CircuitBreakerOpenTimeoutSecs  int  `toml:"circuit_breaker_open_timeout_seconds"` // Seconds before trying half-open (default: 60)

	// DNS resolver configuration
	DNSResolvers        []string `toml:"dns_resolvers"`          // Custom DNS servers (e.g. ["8.8.8.8:53", "1.1.1.1:53"])
	DNSTimeoutSeconds   int      `toml:"dns_timeout_seconds"`    // Timeout for DNS queries
	DNSCacheNegativeTTL int      `toml:"dns_cache_negative_ttl"` // TTL for negative DNS responses (NXDOMAIN, etc.)
}

type CallbacksConfig struct {
	WebhookURL               string  `toml:"webhook_url"`
	AuthToken                string  `toml:"auth_token"`
	TimeoutSeconds           int     `toml:"timeout_seconds"`
	MaxCallbackAgeHours      int     `toml:"max_callback_age_hours"`      // Maximum age before giving up (similar to max_message_age_hours)
	InitialRetryDelaySeconds int     `toml:"initial_retry_delay_seconds"` // Initial retry delay (e.g., 30s)
	MaxRetryDelaySeconds     int     `toml:"max_retry_delay_seconds"`     // Maximum retry delay (e.g., 3600s = 1 hour)
	BackoffMultiplier        float64 `toml:"backoff_multiplier"`          // Exponential backoff multiplier (e.g., 2.0)
	BatchSize                int     `toml:"batch_size"`                  // Number of callbacks to process per iteration
}

type ReputationConfig struct {
	AlertWebhookURL        string `toml:"alert_webhook_url"`         // URL to send reputation alerts
	AlertAuthToken         string `toml:"alert_auth_token"`          // Auth token for alert webhook
	AlertTimeoutSeconds    int    `toml:"alert_timeout_seconds"`     // Timeout for alert requests
	DegradedRetryHours     int    `toml:"degraded_retry_hours"`      // Hours before retrying degraded IP (default: 48)
	EnableIPTracking       bool   `toml:"enable_ip_tracking"`        // Enable IP reputation tracking (default: true)
	DegradedIPCleanupHours int    `toml:"degraded_ip_cleanup_hours"` // Hours to keep degraded IP history (default: 168 = 7 days)
}

type GossipConfig struct {
	Enabled       bool     `toml:"enabled"`        // Enable gossip protocol for distributed coordination (default: false)
	BindPort      int      `toml:"bind_port"`      // Port for gossip protocol (default: 7946)
	JoinAddresses []string `toml:"join_addresses"` // Initial seed nodes to join (e.g., ["fune-1:7946", "fune-2:7946"])
	NodeID        string   `toml:"node_id"`        // Unique node identifier (defaults to hostname)
}

// SetDefaults sets default values for optional config fields
func (c *Config) SetDefaults() {
	// Server defaults
	if c.Server.PIDFile == "" {
		c.Server.PIDFile = "fune.pid"
	}

	// Logging defaults
	if c.Logging.Level == "" {
		c.Logging.Level = "info"
	}
	if c.Logging.Format == "" {
		c.Logging.Format = "console"
	}

	// HTTP defaults
	if c.HTTP.MaxBodySizeBytes == 0 {
		c.HTTP.MaxBodySizeBytes = 35 * 1024 * 1024 // 35 MB default
	}
	if c.HTTP.ReadTimeoutSecs == 0 {
		c.HTTP.ReadTimeoutSecs = 30 // 30 seconds
	}
	if c.HTTP.WriteTimeoutSecs == 0 {
		c.HTTP.WriteTimeoutSecs = 30 // 30 seconds
	}
	if c.HTTP.IdleTimeoutSecs == 0 {
		c.HTTP.IdleTimeoutSecs = 120 // 2 minutes
	}
	if c.HTTP.MetricsPath == "" {
		c.HTTP.MetricsPath = "/metrics"
	}
	// Metrics enabled by default
	// Set MetricsEnabled explicitly to false to disable

	// Idempotency defaults
	if c.HTTP.IdempotencyHeader == "" {
		c.HTTP.IdempotencyHeader = "X-Idempotency-Key"
	}
	if c.HTTP.IdempotencyTTLHours == 0 {
		c.HTTP.IdempotencyTTLHours = 24
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
		c.Queue.PollIntervalSeconds = 30 // Poll every 30s as fallback
	}
	if c.Delivery.MXCacheTTLSeconds == 0 {
		c.Delivery.MXCacheTTLSeconds = 3600
	}
	if c.Delivery.ConnectionTimeoutSeconds == 0 {
		c.Delivery.ConnectionTimeoutSeconds = 30
	}
	if c.Delivery.SMTPTimeoutSeconds == 0 {
		c.Delivery.SMTPTimeoutSeconds = 60
	}
	if c.Delivery.MaxMessageAgeHours == 0 {
		c.Delivery.MaxMessageAgeHours = 48
	}
	if c.Delivery.InitialRetryDelaySeconds == 0 {
		c.Delivery.InitialRetryDelaySeconds = 300
	}
	if c.Delivery.MaxRetryDelaySeconds == 0 {
		c.Delivery.MaxRetryDelaySeconds = 43200
	}
	if c.Delivery.BackoffMultiplier == 0 {
		c.Delivery.BackoffMultiplier = 2.0
	}
	if c.Delivery.GreylistRetryDelaySeconds == 0 {
		c.Delivery.GreylistRetryDelaySeconds = 120
	}
	if c.Delivery.IPSelection == "" {
		c.Delivery.IPSelection = "round-robin"
	}
	if c.Delivery.MaxIPsPerMX == 0 {
		c.Delivery.MaxIPsPerMX = 5
	}
	if c.Delivery.PerDomainIntervalSeconds == 0 {
		c.Delivery.PerDomainIntervalSeconds = 2
	}
	if c.Delivery.PerDomainRetrySeconds == 0 {
		c.Delivery.PerDomainRetrySeconds = 5
	}
	// Circuit breaker defaults (enabled by default for production safety)
	// Set CircuitBreakerEnabled explicitly to false to disable
	if c.Delivery.CircuitBreakerFailureThreshold == 0 {
		c.Delivery.CircuitBreakerFailureThreshold = 5
	}
	if c.Delivery.CircuitBreakerSuccessThreshold == 0 {
		c.Delivery.CircuitBreakerSuccessThreshold = 2
	}
	if c.Delivery.CircuitBreakerOpenTimeoutSecs == 0 {
		c.Delivery.CircuitBreakerOpenTimeoutSecs = 60
	}
	if c.Callbacks.TimeoutSeconds == 0 {
		c.Callbacks.TimeoutSeconds = 10
	}
	if c.Callbacks.MaxCallbackAgeHours == 0 {
		c.Callbacks.MaxCallbackAgeHours = 48 // Same as message age by default
	}
	if c.Callbacks.InitialRetryDelaySeconds == 0 {
		c.Callbacks.InitialRetryDelaySeconds = 30 // 30 seconds
	}
	if c.Callbacks.MaxRetryDelaySeconds == 0 {
		c.Callbacks.MaxRetryDelaySeconds = 3600 // 1 hour max
	}
	if c.Callbacks.BackoffMultiplier == 0 {
		c.Callbacks.BackoffMultiplier = 2.0
	}
	if c.Callbacks.BatchSize == 0 {
		c.Callbacks.BatchSize = 10
	}
	// DNS defaults
	if c.Delivery.DNSTimeoutSeconds == 0 {
		c.Delivery.DNSTimeoutSeconds = 5
	}
	if c.Delivery.DNSCacheNegativeTTL == 0 {
		c.Delivery.DNSCacheNegativeTTL = 60 // 1 minute for failures
	}
	// If no DNS resolvers specified, use system default
	if len(c.Delivery.DNSResolvers) == 0 {
		c.Delivery.DNSResolvers = []string{} // Empty means use system resolver
	}

	// Reputation defaults
	if c.Reputation.AlertTimeoutSeconds == 0 {
		c.Reputation.AlertTimeoutSeconds = 10
	}
	if c.Reputation.DegradedRetryHours == 0 {
		c.Reputation.DegradedRetryHours = 48 // 48 hours default
	}
	if c.Reputation.DegradedIPCleanupHours == 0 {
		c.Reputation.DegradedIPCleanupHours = 168 // 7 days
	}
	// EnableIPTracking is enabled by default (set to false to disable)

	// Gossip defaults
	if c.Gossip.BindPort == 0 {
		c.Gossip.BindPort = 7946 // Default memberlist port
	}
	// Gossip is disabled by default (set enabled=true to enable)
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

	// Apply defaults
	config.SetDefaults()

	return &config, nil
}
