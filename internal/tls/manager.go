// Package tls provides TLS certificate management for HTTPS/TLS termination,
// supporting both file-based certificates and automatic Let's Encrypt certificates
// with cluster-aware coordination.
//
// Key Features:
//   - Automatic Let's Encrypt certificate issuance and renewal
//   - S3-based certificate storage for multi-node cluster deployments
//   - Leader-based coordination to prevent duplicate ACME challenges
//   - Certificate expiration monitoring and alerting
//   - HTTP-01 ACME challenge handling for domain verification
//   - Automatic retry and exponential backoff for S3 operations
//
// Architecture:
//
// The TLS manager integrates with golang.org/x/crypto/acme/autocert for ACME
// protocol handling. In multi-node clusters, the gossip service determines the
// leader node, which is responsible for issuing and renewing certificates.
// Non-leader nodes read certificates from S3 but do not perform writes.
//
// Certificate storage uses a custom S3 cache implementation that respects leader
// election, preventing race conditions during certificate issuance. Certificates
// are validated at startup to ensure S3 bucket accessibility.
//
// Example Usage:
//
//	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
//	defer cancel()
//
//	manager, err := tls.NewManager(ctx, tlsConfig, gossipSvc, logger)
//	if err != nil {
//		log.Fatal(err)
//	}
//
//	// Use with HTTP server
//	server := &http.Server{
//		TLSConfig: manager.TLSConfig(),
//		Handler:   manager.HTTPHandler(mainHandler),
//	}
//
//	// Serve HTTP-01 challenges on port 80
//	go http.ListenAndServe(":80", manager.HTTPHandler(nil))
//
//	// Serve HTTPS on port 443
//	server.ListenAndServeTLS("", "")
package tls

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"fune/internal/config"
	"fune/internal/gossip"
	"fune/internal/tls/storage"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/retry"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"golang.org/x/crypto/acme/autocert"
)

// Manager orchestrates TLS certificate management using Let's Encrypt and S3
// storage. It coordinates certificate issuance across cluster nodes using
// leader election to prevent duplicate ACME challenges.
type Manager struct {
	autocertManager *autocert.Manager
	logger          *slog.Logger
	domains         []string // Track configured domains for monitoring
}

// NewManager creates a new TLS manager for Let's Encrypt certificate management.
// Returns nil if TLS is disabled or provider is not "letsencrypt".
//
// Storage Backends:
//   - "s3": Requires gossip service for leader-based coordination in multi-node clusters.
//     The context is used for S3 initialization and bucket validation with a 10-second
//     timeout. If bucket validation fails, returns an error before starting the manager.
//   - "file": Uses local filesystem storage (autocert.DirCache). Suitable for single-node
//     deployments. Does not require gossip service.
//
// Parameters:
//   - ctx: Context for S3 initialization (timeout recommended, ignored for file storage)
//   - cfg: TLS configuration with Let's Encrypt settings
//   - gossipSvc: Gossip service for leader election (required for S3 storage, optional for file)
//   - logger: Structured logger for certificate operations
//
// Returns nil manager if using S3 storage and gossip service is unavailable, as leader
// coordination cannot function without it.
func NewManager(ctx context.Context, cfg *config.TLSConfig, gossipSvc *gossip.Gossip, logger *slog.Logger) (*Manager, error) {
	if !cfg.Enabled || cfg.Provider != "letsencrypt" {
		return nil, nil
	}

	var cache autocert.Cache

	switch cfg.LetsEncrypt.StorageProvider {
	case "s3":
		// Validate gossip service for leader-based coordination in S3 mode
		if gossipSvc == nil {
			logger.Warn("gossip service not available, TLS manager will not use leader coordination")
			return nil, nil
		}

		// Create S3 cache with context for proper cancellation and timeout handling
		s3Cache, err := createS3Cache(ctx, cfg.LetsEncrypt, gossipSvc.IsLeader, logger)
		if err != nil {
			return nil, err
		}
		cache = s3Cache

	case "file":
		// Use autocert.DirCache for local filesystem storage
		cache = autocert.DirCache(cfg.LetsEncrypt.CacheDir)
		logger.Info("using file-based certificate cache",
			"cache_dir", cfg.LetsEncrypt.CacheDir)

	default:
		return nil, fmt.Errorf("unsupported storage provider: %s (must be 's3' or 'file')", cfg.LetsEncrypt.StorageProvider)
	}

	m := &autocert.Manager{
		Prompt:     autocert.AcceptTOS,
		HostPolicy: autocert.HostWhitelist(cfg.LetsEncrypt.Domains...),
		Cache:      cache,
		Email:      cfg.LetsEncrypt.Email,
	}

	logger.Info("TLS manager initialized",
		"domains", cfg.LetsEncrypt.Domains,
		"email", cfg.LetsEncrypt.Email,
		"storage", cfg.LetsEncrypt.StorageProvider)

	return &Manager{
		autocertManager: m,
		logger:          logger,
		domains:         cfg.LetsEncrypt.Domains,
	}, nil
}

// TLSConfig returns a TLS configuration for use with http.Server. The returned
// config includes automatic certificate selection via SNI (Server Name Indication)
// and handles certificate issuance/renewal transparently.
//
// Returns nil if the manager is not initialized (TLS disabled).
func (m *Manager) TLSConfig() *tls.Config {
	if m == nil || m.autocertManager == nil {
		return nil
	}
	return m.autocertManager.TLSConfig()
}

// HTTPHandler returns an HTTP handler for ACME HTTP-01 challenges. This handler
// must be served on port 80 for Let's Encrypt domain verification. If the incoming
// request is not an ACME challenge (/.well-known/acme-challenge/*), it delegates
// to the fallback handler (which can be nil to return 404).
//
// Example:
//
//	// Serve ACME challenges on port 80
//	go http.ListenAndServe(":80", manager.HTTPHandler(nil))
func (m *Manager) HTTPHandler(fallback http.Handler) http.Handler {
	if m == nil || m.autocertManager == nil {
		return fallback
	}
	return m.autocertManager.HTTPHandler(fallback)
}

// CertificateInfo contains information about a TLS certificate including
// validity period and expiration status. Used for monitoring and alerting.
type CertificateInfo struct {
	Domain          string
	NotBefore       time.Time
	NotAfter        time.Time
	DaysUntilExpiry int
	IsExpired       bool
	Error           error // Set if certificate retrieval or parsing failed
}

// GetCertificateInfo retrieves certificate information for a domain by querying
// the autocert manager. This triggers certificate retrieval from cache or issuance
// if not present. Returns CertificateInfo with Error set if retrieval fails.
//
// Useful for monitoring certificate expiration and validation status.
func (m *Manager) GetCertificateInfo(domain string) CertificateInfo {
	info := CertificateInfo{
		Domain: domain,
	}

	if m == nil || m.autocertManager == nil {
		info.Error = fmt.Errorf("TLS manager not initialized")
		return info
	}

	// Create a ClientHello to trigger certificate retrieval
	hello := &tls.ClientHelloInfo{
		ServerName: domain,
	}

	cert, err := m.autocertManager.GetCertificate(hello)
	if err != nil {
		info.Error = fmt.Errorf("failed to get certificate: %w", err)
		return info
	}

	// Parse the leaf certificate if not already parsed
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

// CheckCertificates checks all configured domains and logs their certificate
// status (validity, expiration). Logs warnings for certificates expiring within
// 30 days and errors for expired certificates. Useful for periodic health checks.
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
				"expired_at", info.NotAfter,
				"days_overdue", -info.DaysUntilExpiry)
		} else if info.DaysUntilExpiry <= 7 {
			m.logger.Warn("certificate expiring soon",
				"domain", domain,
				"expires_at", info.NotAfter,
				"days_remaining", info.DaysUntilExpiry)
		} else if info.DaysUntilExpiry <= 30 {
			m.logger.Info("certificate status",
				"domain", domain,
				"expires_at", info.NotAfter,
				"days_remaining", info.DaysUntilExpiry)
		} else {
			m.logger.Debug("certificate status",
				"domain", domain,
				"expires_at", info.NotAfter,
				"days_remaining", info.DaysUntilExpiry)
		}
	}
}

func createS3Cache(ctx context.Context, cfg config.LetsEncryptConfig, isLeaderF func() bool, logger *slog.Logger) (*storage.S3Cache, error) {
	// Create a timeout context for S3 initialization (10 seconds)
	initCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	// Load default AWS config with retry configuration
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

	// Override region if specified
	if cfg.S3.Region != "" {
		awsCfg.Region = cfg.S3.Region
	}

	// Use static credentials if provided, otherwise fallback to default credential chain
	if cfg.S3.AccessKeyID != "" && cfg.S3.SecretAccessKey != "" {
		awsCfg.Credentials = credentials.NewStaticCredentialsProvider(
			cfg.S3.AccessKeyID,
			cfg.S3.SecretAccessKey,
			"",
		)
	}

	s3Client := s3.NewFromConfig(awsCfg)

	// Validate S3 bucket exists and is accessible
	logger.Info("validating S3 bucket access", "bucket", cfg.S3.Bucket)
	_, err = s3Client.HeadBucket(initCtx, &s3.HeadBucketInput{
		Bucket: &cfg.S3.Bucket,
	})
	if err != nil {
		return nil, fmt.Errorf("S3 bucket validation failed for '%s': %w (check bucket exists and IAM permissions)", cfg.S3.Bucket, err)
	}

	logger.Info("S3 bucket validated successfully", "bucket", cfg.S3.Bucket)

	return &storage.S3Cache{
		S3Client:  s3Client,
		Bucket:    cfg.S3.Bucket,
		IsLeaderF: isLeaderF,
		Logger:    logger,
	}, nil
}
