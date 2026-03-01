// Package config provides TOML-based configuration management for the Strela SMTP delivery service.
package config

import (
	"fmt"
	"net"
	"os"
	"strconv"

	"github.com/BurntSushi/toml"
)

// Config is the root configuration structure containing all service settings.
type Config struct {
	Logging    LoggingConfig    `toml:"logging"`
	Inbound    InboundConfig    `toml:"inbound"`
	TLS        TLSConfig        `toml:"tls"`
	DNS        DNSConfig        `toml:"dns"`
	Metrics    MetricsConfig    `toml:"metrics"`
	Admin      AdminConfig      `toml:"admin"`
	Outbound   OutboundConfig   `toml:"outbound"`
	Reputation ReputationConfig `toml:"reputation"`
	Cluster    ClusterConfig    `toml:"cluster"`
	DKIM       DKIMConfig       `toml:"dkim"`
	ARC        ARCConfig        `toml:"arc"`
	SRS        SRSConfig        `toml:"srs"`
}

// LoggingConfig controls logging behavior for the service.
type LoggingConfig struct {
	Output string `toml:"output"` // Log destination: stderr, stdout, syslog, or /path/to/file.log (default: stderr)
	Format string `toml:"format"` // Log format: console, json (default: console)
	Level  string `toml:"level"`  // Log level: debug, info, warn, error (default: info)
}

// InboundConfig configures the HTTP API server that accepts message submissions.
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

	// Concurrency limiting
	MaxConcurrentRequests int `toml:"max_concurrent_requests"` // Maximum concurrent HTTP requests (0 = unlimited)
}

// MetricsConfig configures the Prometheus metrics endpoint (served on the admin server).
type MetricsConfig struct {
	Enabled bool   `toml:"enabled"` // Enable Prometheus metrics endpoint (default: true)
	Path    string `toml:"path"`    // Path for metrics endpoint (default: /metrics)
}

// AdminConfig configures the admin server (health + metrics endpoints).
// This server listens on a separate localhost-only address to avoid public exposure.
type AdminConfig struct {
	Enabled    bool   `toml:"enabled"`     // Enable admin server with health + metrics (default: true)
	ListenAddr string `toml:"listen_addr"` // Address for admin server (default: 127.0.0.1:8080)
	Username   string `toml:"username"`    // HTTP Basic Auth username (optional)
	Password   string `toml:"password"`    // HTTP Basic Auth password (optional)
}

// TLSConfig configures TLS/SSL certificate handling for HTTPS.
type TLSConfig struct {
	Enabled     bool              `toml:"enabled"`
	CertFile    string            `toml:"cert_file"`
	KeyFile     string            `toml:"key_file"`
	Provider    string            `toml:"provider"` // "file" or "letsencrypt" (default: file)
	LetsEncrypt LetsEncryptConfig `toml:"letsencrypt"`
}

// LetsEncryptConfig configures automatic certificate provisioning via Let's Encrypt.
type LetsEncryptConfig struct {
	Email               string   `toml:"email"`
	Domains             []string `toml:"domains"`
	DefaultDomain       string   `toml:"default_domain"`        // Default domain for SNI-less connections (optional, uses first domain if not specified)
	StorageProvider     string   `toml:"storage_provider"`      // "s3" or "file" (default: s3)
	CacheDir            string   `toml:"cache_dir"`             // Directory for local file cache (used for "file" mode or as local cache when using S3)
	SyncIntervalMinutes int      `toml:"sync_interval_minutes"` // Interval for syncing local cache to S3 (default: 5 minutes, only applies when storage_provider="s3")
	S3                  S3Config `toml:"s3"`
}

// S3Config provides AWS S3 credentials for Let's Encrypt certificate storage.
type S3Config struct {
	Bucket    string `toml:"bucket"`
	Region    string `toml:"region"`
	Endpoint  string `toml:"endpoint"` // Custom S3 endpoint (e.g., for Backblaze B2)
	Prefix    string `toml:"prefix"`   // Base prefix for all S3 keys (e.g., "myapp/" → myapp/certs/, myapp/stats/, etc.)
	AccessKey string `toml:"access_key"`
	SecretKey string `toml:"secret_key"`
}

// DNSConfig configures DNS resolution and caching behavior.
type DNSConfig struct {
	Resolvers               []string `toml:"resolvers"`                  // Custom DNS servers (empty = system default)
	TimeoutSeconds          int      `toml:"timeout_seconds"`            // Timeout for DNS queries (default: 5s)
	CacheTTLSeconds         int      `toml:"cache_ttl_seconds"`          // Cache successful DNS lookups (default: 3600s)
	CacheNegativeTTLSeconds int      `toml:"cache_negative_ttl_seconds"` // Cache failed lookups/NXDOMAIN (default: 60s)
}

// OutboundConfig configures SMTP delivery behavior and settings.
type OutboundConfig struct {
	// Source IP configuration - supports individual IPs and CIDR subnets
	// Examples: ["192.0.2.1", "192.0.2.0/24", "2001:db8::1", "2001:db8::/64"]
	SourceIPsV4       []string `toml:"source_ips_v4"`       // IPv4 source IPs/subnets (expanded on startup)
	SourceIPsV6       []string `toml:"source_ips_v6"`       // IPv6 source IPs/subnets (expanded on startup)
	PreferIPv6        bool     `toml:"prefer_ipv6"`         // Try IPv6 first, fallback to IPv4 (default: true)
	SourceIPSelection string   `toml:"source_ip_selection"` // "round-robin", "random", "hash-domain" (default: round-robin)

	MXCacheTTLSeconds        int    `toml:"mx_cache_ttl_seconds"`        // MX record cache TTL (default: 3600s)
	ConnectionPoolTTLSeconds int    `toml:"connection_pool_ttl_seconds"` // Max time to keep idle connection (default: 5s)
	ConnectionTimeoutSeconds int    `toml:"connection_timeout_seconds"`  // TCP connection timeout (default: 15s)
	SMTPTimeoutSeconds       int    `toml:"smtp_timeout_seconds"`        // SMTP command timeout (default: 60s)
	DeliveryTimeoutSeconds   int    `toml:"delivery_timeout_seconds"`    // Maximum time to wait for SMTP delivery (default: 30s)
	MaxIPsPerMX              int    `toml:"max_ips_per_mx"`              // Maximum number of IPs to try per MX host (default: 5)
	HelloHostname            string `toml:"hello_hostname"`              // Hostname for EHLO greeting (default: system hostname)

	// Rate limiting per destination domain
	PerDomainIntervalSeconds int      `toml:"per_domain_interval_seconds"` // Minimum seconds between deliveries to same domain (default: 2s)
	PerDomainBurst           int      `toml:"per_domain_burst"`            // Bucket size for token bucket rate limiting (default: 10)
	PerDomainRetrySeconds    int      `toml:"per_domain_retry_seconds"`    // Delay before retrying throttled message (default: 5s)
	RateLimitWhitelist       []string `toml:"rate_limit_whitelist"`        // Domains exempt from rate limiting (default: [])
}

// ReputationConfig configures IP reputation tracking and alerting.
type ReputationConfig struct {
	AlertWebhookURL        string `toml:"alert_webhook_url"`         // Webhook URL for reputation alerts
	AlertAuthToken         string `toml:"alert_auth_token"`          // Bearer token for alert webhook
	AlertTimeoutSeconds    int    `toml:"alert_timeout_seconds"`     // Alert webhook timeout (default: 10s)
	DegradedRetryHours     int    `toml:"degraded_retry_hours"`      // Hours before retrying degraded IP (default: 48h)
	EnableIPTracking       bool   `toml:"enable_ip_tracking"`        // Enable IP reputation tracking (default: false)
	DegradedIPCleanupHours int    `toml:"degraded_ip_cleanup_hours"` // Hours before cleaning up old degraded IPs (default: 168h/7d)
}

// ClusterConfig configures the gossip protocol for multi-node clustering.
type ClusterConfig struct {
	Enabled   bool     `toml:"enabled"`
	Bind      string   `toml:"bind"`       // Gossip bind address, can be "IP:port" or just "IP" (default: 0.0.0.0)
	Port      int      `toml:"port"`       // Gossip port, used if not specified in bind (default: 7946)
	Peers     []string `toml:"peers"`      // All other cluster nodes (address:port)
	NodeID    string   `toml:"node_id"`    // Unique node identifier (hostname recommended)
	SecretKey string   `toml:"secret_key"` // 32-byte base64 encoded encryption key for AES-256
}

// GetBindAddr returns the bind address by parsing the bind field.
func (c *ClusterConfig) GetBindAddr() string {
	if c.Bind == "" {
		return "0.0.0.0"
	}
	// If bind contains port, split it
	if host, _, err := net.SplitHostPort(c.Bind); err == nil {
		return host
	}
	return c.Bind
}

// GetBindPort returns the bind port from either bind or port field.
func (c *ClusterConfig) GetBindPort() int {
	if c.Bind != "" {
		if _, portStr, err := net.SplitHostPort(c.Bind); err == nil {
			if port, err := strconv.Atoi(portStr); err == nil {
				return port
			}
		}
	}
	if c.Port > 0 {
		return c.Port
	}
	return 7946
}

// DKIMConfig configures DKIM (DomainKeys Identified Mail) signing for outbound messages.
type DKIMConfig struct {
	Enabled        bool   `toml:"enabled"`          // Enable DKIM signing (default: false)
	Selector       string `toml:"selector"`         // DNS selector for DKIM public key (e.g., "default", "mail")
	Domain         string `toml:"domain"`           // Domain for DKIM signing (e.g., "example.com")
	PrivateKeyPath string `toml:"private_key_path"` // Path to RSA private key in PEM format (1024 or 2048 bits)
	SkipValidation bool   `toml:"skip_validation"`  // Skip DNS validation of DKIM record (default: false)
	HeaderCanon    string `toml:"header_canon"`     // Header canonicalization: "relaxed" or "simple" (default: relaxed)
	BodyCanon      string `toml:"body_canon"`       // Body canonicalization: "relaxed" or "simple" (default: relaxed)
}

// ARCConfig configures Authenticated Received Chain (ARC) signing for email forwarding.
type ARCConfig struct {
	Enabled        bool   `toml:"enabled"`          // Enable ARC signing (default: false)
	Selector       string `toml:"selector"`         // DNS selector for ARC public key (e.g., "arc-2024")
	Domain         string `toml:"domain"`           // Domain for ARC signing (e.g., "example.com")
	PrivateKeyPath string `toml:"private_key_path"` // Path to RSA private key in PEM format
	HeaderCanon    string `toml:"header_canon"`     // Header canonicalization: "relaxed" or "simple" (default: relaxed)
	BodyCanon      string `toml:"body_canon"`       // Body canonicalization: "relaxed" or "simple" (default: relaxed)
}

// SRSConfig configures Sender Rewriting Scheme (SRS) for envelope sender rewriting.
type SRSConfig struct {
	Enabled             bool     `toml:"enabled"`               // Enable SRS envelope rewriting (default: false)
	Domains             []string `toml:"domains"`               // List of domains for rewritten addresses (e.g., ["srs1.example.com", "srs2.example.com"])
	Selection           string   `toml:"selection"`             // Domain selection strategy: "round-robin" or "hash-sender" (default: round-robin)
	Secret              string   `toml:"secret"`                // Secret key for HMAC hash (min 16 chars, keep secure)
	MaxAge              int      `toml:"max_age"`               // Maximum age in days for SRS addresses (default: 21)
	HashLength          int      `toml:"hash_length"`           // Length of hash in SRS address (default: 4, range: 2-8)
	Separator           string   `toml:"separator"`             // Separator character (default: "=")
	SkipDomains         []string `toml:"skip_domains"`          // Destination domains to skip SRS rewriting (e.g., ["gmail.com", "googlemail.com"])
	SkipIfDKIMPass      bool     `toml:"skip_if_dkim_pass"`     // Skip SRS rewriting when caller reports DKIM=pass for the message (default: false)
	SkipIfSameDomain    bool     `toml:"skip_if_same_domain"`   // Skip SRS rewriting when sender domain matches recipient domain (default: false)
	UseDynamicSubdomain bool     `toml:"use_dynamic_subdomain"` // Prepend sanitized sender domain as subdomain of selected SRS domain (e.g. outlook-com.srs.example.com) (default: false)
}

// SetDefaults sets default values for optional configuration fields.
func (c *Config) SetDefaults() {
	if c.Logging.Output == "" {
		c.Logging.Output = "stderr"
	}
	if c.Logging.Format == "" {
		c.Logging.Format = "console"
	}
	if c.Logging.Level == "" {
		c.Logging.Level = "info"
	}
	if c.Inbound.MaxBodySizeBytes == 0 {
		c.Inbound.MaxBodySizeBytes = 35 * 1024 * 1024
	}
	if c.Inbound.ReadTimeoutSecs == 0 {
		c.Inbound.ReadTimeoutSecs = 30
	}
	if c.Inbound.WriteTimeoutSecs == 0 {
		c.Inbound.WriteTimeoutSecs = 90 // Must exceed delivery_timeout_seconds (default 30s) + margin
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

	// Metrics defaults
	if c.Metrics.Path == "" {
		c.Metrics.Path = "/metrics"
	}

	// Admin server defaults (localhost-only for security)
	if c.Admin.ListenAddr == "" {
		c.Admin.ListenAddr = "127.0.0.1:8080"
	}

	if c.TLS.Provider == "" {
		c.TLS.Provider = "file"
	}
	if c.TLS.LetsEncrypt.StorageProvider == "" {
		c.TLS.LetsEncrypt.StorageProvider = "s3"
	}
	if c.TLS.LetsEncrypt.CacheDir == "" {
		c.TLS.LetsEncrypt.CacheDir = "./letsencrypt-cache"
	}
	// Normalize S3 prefix: strip leading slashes and ensure trailing slash
	if c.TLS.LetsEncrypt.S3.Prefix != "" {
		originalPrefix := c.TLS.LetsEncrypt.S3.Prefix
		// Strip leading slashes
		for len(c.TLS.LetsEncrypt.S3.Prefix) > 0 && c.TLS.LetsEncrypt.S3.Prefix[0] == '/' {
			c.TLS.LetsEncrypt.S3.Prefix = c.TLS.LetsEncrypt.S3.Prefix[1:]
		}
		// Add trailing slash if not empty after stripping
		if c.TLS.LetsEncrypt.S3.Prefix != "" && c.TLS.LetsEncrypt.S3.Prefix[len(c.TLS.LetsEncrypt.S3.Prefix)-1] != '/' {
			c.TLS.LetsEncrypt.S3.Prefix = c.TLS.LetsEncrypt.S3.Prefix + "/"
		}
		// Log normalization if changed
		if originalPrefix != c.TLS.LetsEncrypt.S3.Prefix {
			// Can't log here since we don't have a logger, but the manager will log the final value
		}
	}
	if c.Outbound.MXCacheTTLSeconds == 0 {
		c.Outbound.MXCacheTTLSeconds = 3600
	}
	if c.Outbound.ConnectionPoolTTLSeconds == 0 {
		c.Outbound.ConnectionPoolTTLSeconds = 5
	}
	if c.Outbound.ConnectionTimeoutSeconds == 0 {
		c.Outbound.ConnectionTimeoutSeconds = 15
	}
	if c.Outbound.SMTPTimeoutSeconds == 0 {
		c.Outbound.SMTPTimeoutSeconds = 60
	}
	if c.Outbound.DeliveryTimeoutSeconds == 0 {
		c.Outbound.DeliveryTimeoutSeconds = 30
	}
	if c.Outbound.SourceIPSelection == "" {
		c.Outbound.SourceIPSelection = "round-robin"
	}
	// Default to preferring IPv6 (only matters if not explicitly set)
	// Note: TOML bool defaults to false, so we can't distinguish between unset and false
	// We'll treat false as "don't prefer" and true as "prefer"
	// Actually, let's not set a default - user must be explicit
	if c.Outbound.MaxIPsPerMX == 0 {
		c.Outbound.MaxIPsPerMX = 5
	}
	if c.Outbound.HelloHostname == "" {
		hostname, _ := os.Hostname()
		if hostname == "" {
			hostname = "localhost"
		}
		c.Outbound.HelloHostname = hostname
	}
	if c.Outbound.PerDomainIntervalSeconds == 0 {
		c.Outbound.PerDomainIntervalSeconds = 2
	}
	if c.Outbound.PerDomainBurst == 0 {
		c.Outbound.PerDomainBurst = 10
	}
	if c.Outbound.PerDomainRetrySeconds == 0 {
		c.Outbound.PerDomainRetrySeconds = 5
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
	if c.Reputation.AlertTimeoutSeconds == 0 {
		c.Reputation.AlertTimeoutSeconds = 10
	}
	if c.Reputation.DegradedRetryHours == 0 {
		c.Reputation.DegradedRetryHours = 48
	}
	if c.Reputation.DegradedIPCleanupHours == 0 {
		c.Reputation.DegradedIPCleanupHours = 168
	}
	// Cluster defaults are handled by GetBindAddr()/GetBindPort() methods
	if c.ARC.HeaderCanon == "" {
		c.ARC.HeaderCanon = "relaxed"
	}
	if c.ARC.BodyCanon == "" {
		c.ARC.BodyCanon = "relaxed"
	}
	if c.SRS.MaxAge == 0 {
		c.SRS.MaxAge = 21
	}
	if c.SRS.HashLength == 0 {
		c.SRS.HashLength = 4
	}
	if c.SRS.Separator == "" {
		c.SRS.Separator = "="
	}
	if c.SRS.Selection == "" {
		c.SRS.Selection = "round-robin"
	}
}

// LoadConfig loads and parses a TOML configuration file from the given path.
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
