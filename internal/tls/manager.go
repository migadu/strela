package tls

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"strela/internal/config"
	"strela/internal/tls/storage"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/retry"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"golang.org/x/crypto/acme/autocert"
)

// Manager orchestrates TLS certificate management using Let's Encrypt.
type Manager struct {
	autocertManager *autocert.Manager
	logger          *slog.Logger
	domains         []string
	defaultDomain   string                  // Default domain for SNI-less connections
	syncWorker      *storage.CertSyncWorker // Periodic S3 sync worker (nil if not using S3)
	tlsConfig       *tls.Config             // Cached TLS config with wrapped GetCertificate
}

// NewManager creates a new TLS manager.
// If isLeaderF is provided and non-nil, only the cluster leader is allowed to
// request new certificates from Let's Encrypt (prevents race conditions and
// rate limit exhaustion across multiple instances).
func NewManager(ctx context.Context, cfg *config.TLSConfig, logger *slog.Logger, isLeaderF ...func() bool) (*Manager, error) {
	if !cfg.Enabled || cfg.Provider != "letsencrypt" {
		return nil, nil
	}

	var cache autocert.Cache
	var syncWorker *storage.CertSyncWorker

	switch cfg.LetsEncrypt.StorageProvider {
	case "s3":
		// Create S3 cache
		s3Cache, err := createS3Cache(ctx, cfg.LetsEncrypt, logger)
		if err != nil {
			return nil, err
		}

		// Create fallback cache (local file + S3) for hybrid storage
		cacheDir := cfg.LetsEncrypt.CacheDir
		if cacheDir == "" {
			cacheDir = "cert-cache" // Default local cache directory
		}
		fallbackCache := storage.NewFallbackCache(cacheDir, s3Cache, logger)
		cache = fallbackCache

		// Start periodic sync worker
		syncInterval := time.Duration(cfg.LetsEncrypt.SyncIntervalMinutes) * time.Minute
		if syncInterval == 0 {
			syncInterval = 5 * time.Minute // Default: 5 minutes
		}
		syncWorker = storage.NewCertSyncWorker(fallbackCache, syncInterval, logger)
		syncWorker.Start()

		prefixInfo := cfg.LetsEncrypt.S3.Prefix
		if prefixInfo == "" {
			prefixInfo = "(none - bucket root)"
		}
		logger.Info("using hybrid file+S3 certificate cache with periodic sync",
			"local_cache_dir", cacheDir,
			"s3_bucket", cfg.LetsEncrypt.S3.Bucket,
			"s3_prefix", prefixInfo,
			"sync_interval", syncInterval)

	case "file":
		// Use autocert.DirCache for local filesystem storage only
		cache = autocert.DirCache(cfg.LetsEncrypt.CacheDir)
		logger.Info("using file-based certificate cache",
			"cache_dir", cfg.LetsEncrypt.CacheDir)

	default:
		return nil, fmt.Errorf("unsupported storage provider: %s", cfg.LetsEncrypt.StorageProvider)
	}

	// Wrap cache with cluster-aware leader gating if a leader function was provided.
	// This ensures only the cluster leader can request new certificates from Let's Encrypt,
	// preventing race conditions and rate limit exhaustion across multiple instances.
	var leaderFunc func() bool
	if len(isLeaderF) > 0 && isLeaderF[0] != nil {
		leaderFunc = isLeaderF[0]
		cache = storage.NewClusterAwareCache(cache, leaderFunc, logger)
		logger.Info("TLS certificate cache wrapped with cluster-aware leader gating")
	} else {
		logger.Info("TLS running in single-instance mode (no cluster leader election)")
	}

	autocertMgr := &autocert.Manager{
		Prompt:     autocert.AcceptTOS,
		HostPolicy: autocert.HostWhitelist(cfg.LetsEncrypt.Domains...),
		Cache:      cache,
		Email:      cfg.LetsEncrypt.Email,
	}

	// Determine default domain for SNI-less connections
	defaultDomain := cfg.LetsEncrypt.DefaultDomain
	if defaultDomain == "" && len(cfg.LetsEncrypt.Domains) > 0 {
		// If not specified, use the first configured domain
		defaultDomain = cfg.LetsEncrypt.Domains[0]
	}

	// Create base TLS config
	baseTLSConfig := autocertMgr.TLSConfig()

	m := &Manager{
		autocertManager: autocertMgr,
		logger:          logger,
		domains:         cfg.LetsEncrypt.Domains,
		defaultDomain:   defaultDomain,
		syncWorker:      syncWorker,
		tlsConfig:       nil, // Will be set below after wrapping GetCertificate
	}

	// Wrap GetCertificate with enhanced logging and SNI handling
	originalGetCert := baseTLSConfig.GetCertificate
	baseTLSConfig.GetCertificate = func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
		serverName := hello.ServerName

		// Handle missing SNI by using default domain
		if serverName == "" {
			if defaultDomain != "" {
				logger.Debug("TLS: missing SNI - using default domain", "domain", defaultDomain)
				serverName = defaultDomain
			} else {
				logger.Debug("TLS: rejected certificate request - missing SNI and no default domain")
				return nil, ErrMissingServerName
			}
		}

		// Normalize server name to lowercase for case-insensitive comparison
		// RFC 4343: DNS names are case-insensitive
		serverName = strings.ToLower(serverName)

		// Check if someone is trying to use an IP address (common misconfiguration)
		// Let's Encrypt doesn't issue certificates for IP addresses, only domain names
		if isIPAddress(serverName) {
			logger.Debug("TLS: rejected certificate request for IP address (Let's Encrypt requires domain names)",
				"ip", serverName,
				"remote_addr", hello.Conn.RemoteAddr().String())
			return nil, fmt.Errorf("%w: IP addresses not supported (use domain name)", ErrHostNotAllowed)
		}

		// Check if the server name matches our configured domains using the HostPolicy
		if err := autocertMgr.HostPolicy(nil, serverName); err != nil {
			logger.Debug("TLS: rejected certificate request for unconfigured domain",
				"domain", serverName,
				"remote_addr", hello.Conn.RemoteAddr().String(),
				"error", err)
			return nil, fmt.Errorf("%w: %s", ErrHostNotAllowed, serverName)
		}

		logger.Debug("TLS: certificate request during handshake", "domain", serverName, "has_sni", hello.ServerName != "")

		// Create a modified ClientHelloInfo with the resolved server name
		modifiedHello := *hello
		modifiedHello.ServerName = serverName

		cert, err := originalGetCert(&modifiedHello)
		if err != nil {
			// Certificate retrieval failures are often transient (S3 down, ACME rate limits, network issues)
			// Wrap as ErrCertificateUnavailable so the server logs but doesn't crash
			// This allows the server to continue serving cached certificates for other domains
			logger.Error("TLS: failed to get certificate",
				"server_name", serverName,
				"error", err,
				"error_type", fmt.Sprintf("%T", err))
			return nil, fmt.Errorf("%w for %s: %v", ErrCertificateUnavailable, serverName, err)
		}
		logger.Debug("TLS: certificate provided successfully", "domain", serverName)
		return cert, nil
	}

	// Store the wrapped TLS config in the manager
	m.tlsConfig = baseTLSConfig

	logger.Info("TLS manager initialized",
		"domains", cfg.LetsEncrypt.Domains,
		"email", cfg.LetsEncrypt.Email,
		"storage", cfg.LetsEncrypt.StorageProvider,
		"default_domain", defaultDomain)

	return m, nil
}

// TLSConfig returns the TLS configuration for use with HTTP/SMTP servers.
// Returns the cached config with our wrapped GetCertificate function.
func (m *Manager) TLSConfig() *tls.Config {
	if m == nil {
		return nil
	}
	if m.tlsConfig != nil {
		m.logger.Debug("TLSConfig retrieved (using cached wrapped config)",
			"has_get_certificate", m.tlsConfig.GetCertificate != nil)
	}
	return m.tlsConfig
}

// HTTPHandler returns an HTTP handler for ACME HTTP-01 challenges.
func (m *Manager) HTTPHandler(fallback http.Handler) http.Handler {
	if m == nil || m.autocertManager == nil {
		return fallback
	}
	return m.autocertManager.HTTPHandler(fallback)
}

// CertificateInfo contains information about a TLS certificate.
type CertificateInfo struct {
	Domain          string
	NotBefore       time.Time
	NotAfter        time.Time
	DaysUntilExpiry int
	IsExpired       bool
	Error           error
}

// GetCertificateInfo retrieves certificate information for a domain.
func (m *Manager) GetCertificateInfo(domain string) CertificateInfo {
	info := CertificateInfo{
		Domain: domain,
	}

	if m == nil || m.autocertManager == nil {
		info.Error = fmt.Errorf("TLS manager not initialized")
		return info
	}

	hello := &tls.ClientHelloInfo{
		ServerName: domain,
	}

	cert, err := m.autocertManager.GetCertificate(hello)
	if err != nil {
		info.Error = fmt.Errorf("failed to get certificate: %w", err)
		return info
	}

	if cert.Leaf == nil && len(cert.Certificate) > 0 {
		leaf, err := x509.ParseCertificate(cert.Certificate[0])
		if err != nil {
			info.Error = fmt.Errorf("failed to parse certificate: %w", err)
			return info
		}
		cert.Leaf = leaf
	}

	if cert.Leaf != nil {
		info.NotBefore = cert.Leaf.NotBefore
		info.NotAfter = cert.Leaf.NotAfter
		info.DaysUntilExpiry = int(time.Until(cert.Leaf.NotAfter).Hours() / 24)
		info.IsExpired = time.Now().After(cert.Leaf.NotAfter)
	}

	return info
}

// CheckCertificates checks all configured domains and logs their status.
func (m *Manager) CheckCertificates() {
	if m == nil || m.autocertManager == nil {
		return
	}

	m.logger.Info("checking certificate status for all domains")

	for _, domain := range m.domains {
		info := m.GetCertificateInfo(domain)

		if info.Error != nil {
			m.logger.Warn("certificate check failed",
				"domain", domain,
				"error", info.Error)
			continue
		}

		if info.IsExpired {
			m.logger.Error("certificate EXPIRED",
				"domain", domain,
				"expired_at", info.NotAfter)
		} else if info.DaysUntilExpiry <= 30 {
			m.logger.Info("certificate expiring soon",
				"domain", domain,
				"days_remaining", info.DaysUntilExpiry)
		}
	}
}

// Stop gracefully shuts down the TLS manager and its sync worker.
func (m *Manager) Stop() {
	if m == nil {
		return
	}

	if m.syncWorker != nil {
		m.logger.Info("stopping certificate sync worker")
		m.syncWorker.Stop(10 * time.Second)
	}
}

func createS3Cache(ctx context.Context, cfg config.LetsEncryptConfig, logger *slog.Logger) (*storage.S3Cache, error) {
	initCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	awsCfg, err := awsconfig.LoadDefaultConfig(initCtx,
		awsconfig.WithRetryer(func() aws.Retryer {
			return retry.NewStandard(func(o *retry.StandardOptions) {
				o.MaxAttempts = 3
				o.MaxBackoff = 5 * time.Second
			})
		}),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to load AWS config: %w", err)
	}

	if cfg.S3.Region != "" {
		awsCfg.Region = cfg.S3.Region
	}

	if cfg.S3.AccessKey != "" && cfg.S3.SecretKey != "" {
		awsCfg.Credentials = credentials.NewStaticCredentialsProvider(
			cfg.S3.AccessKey,
			cfg.S3.SecretKey,
			"",
		)
	}

	// Configure S3 client with custom endpoint if specified (e.g., Backblaze B2, MinIO)
	var s3Client *s3.Client
	if cfg.S3.Endpoint != "" {
		s3Client = s3.NewFromConfig(awsCfg, func(o *s3.Options) {
			o.BaseEndpoint = aws.String(cfg.S3.Endpoint)
			o.UsePathStyle = true // Required for non-AWS S3-compatible services like MinIO/B2
		})
		logger.Info("using custom S3 endpoint", "endpoint", cfg.S3.Endpoint)
	} else {
		s3Client = s3.NewFromConfig(awsCfg)
	}

	logger.Info("validating S3 bucket access", "bucket", cfg.S3.Bucket)

	// Retry S3 HeadBucket with exponential backoff to handle DNS startup race conditions
	maxRetries := 5

	for attempt := 0; attempt < maxRetries; attempt++ {
		if attempt > 0 {
			backoff := time.Duration(1<<uint(attempt-1)) * time.Second // 1s, 2s, 4s, 8s
			logger.Info("retrying S3 bucket validation after backoff",
				"attempt", attempt+1,
				"max_retries", maxRetries,
				"backoff", backoff)

			// Context-aware sleep: bail out early if the init context is cancelled
			// instead of sleeping the full backoff duration.
			select {
			case <-time.After(backoff):
			case <-initCtx.Done():
				return nil, fmt.Errorf("S3 bucket validation cancelled during retry backoff: %w", initCtx.Err())
			}
		}

		_, err = s3Client.HeadBucket(initCtx, &s3.HeadBucketInput{
			Bucket: &cfg.S3.Bucket,
		})
		if err == nil {
			break // Success
		}

		logger.Warn("S3 bucket validation failed",
			"attempt", attempt+1,
			"max_retries", maxRetries,
			"error", err)
	}

	if err != nil {
		return nil, fmt.Errorf("S3 bucket validation failed after %d attempts: %w", maxRetries, err)
	}

	logger.Info("S3 bucket validated successfully", "bucket", cfg.S3.Bucket)

	s3Cache := &storage.S3Cache{
		S3Client: s3Client,
		Bucket:   cfg.S3.Bucket,
		Prefix:   cfg.S3.Prefix,
		Logger:   logger,
	}

	// Log S3Cache creation for debugging
	if cfg.S3.Prefix == "" {
		logger.Warn("S3Cache created WITHOUT prefix - certificates will be stored in bucket root!", "bucket", cfg.S3.Bucket)
	} else {
		logger.Info("S3Cache created with prefix", "bucket", cfg.S3.Bucket, "prefix", cfg.S3.Prefix)
	}

	return s3Cache, nil
}

// isIPAddress checks if a string is an IPv4 or IPv6 address.
// Used to detect when clients are trying to connect via IP instead of domain name.
func isIPAddress(host string) bool {
	// net.ParseIP returns nil if it's not a valid IP address
	return net.ParseIP(host) != nil
}
