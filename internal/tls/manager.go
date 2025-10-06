package tls

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
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
	"go.uber.org/zap"
	"golang.org/x/crypto/acme/autocert"
)

// Manager orchestrates TLS certificate management.
type Manager struct {
	autocertManager *autocert.Manager
	logger          *zap.Logger
	domains         []string // Track configured domains for monitoring
}

// NewManager creates a new TLS manager.
// The context is used for S3 initialization and bucket validation, allowing proper
// cancellation and deadline propagation. A timeout context is recommended.
func NewManager(ctx context.Context, cfg *config.TLSConfig, gossipSvc *gossip.Gossip, logger *zap.Logger) (*Manager, error) {
	if !cfg.Enabled || cfg.Provider != "letsencrypt" {
		return nil, nil
	}

	// Validate gossip service for leader-based coordination
	if gossipSvc == nil {
		logger.Warn("gossip service not available, TLS manager will not use leader coordination")
		return nil, nil
	}

	// Create S3 cache with context for proper cancellation and timeout handling
	s3Cache, err := createS3Cache(ctx, cfg.LetsEncrypt, gossipSvc.IsLeader, logger)
	if err != nil {
		return nil, err
	}

	m := &autocert.Manager{
		Prompt:     autocert.AcceptTOS,
		HostPolicy: autocert.HostWhitelist(cfg.LetsEncrypt.Domains...),
		Cache:      s3Cache,
		Email:      cfg.LetsEncrypt.Email,
	}

	logger.Info("TLS manager initialized",
		zap.Strings("domains", cfg.LetsEncrypt.Domains),
		zap.String("email", cfg.LetsEncrypt.Email),
		zap.String("storage", cfg.LetsEncrypt.StorageProvider))

	return &Manager{
		autocertManager: m,
		logger:          logger,
		domains:         cfg.LetsEncrypt.Domains,
	}, nil
}

// TLSConfig returns a TLS configuration for the HTTP server.
func (m *Manager) TLSConfig() *tls.Config {
	if m == nil || m.autocertManager == nil {
		return nil
	}
	return m.autocertManager.TLSConfig()
}

// HTTPHandler returns an HTTP handler for ACME HTTP-01 challenges.
// This handler must be served on port 80 for Let's Encrypt verification.
// If the request is not an ACME challenge, it delegates to the fallback handler.
func (m *Manager) HTTPHandler(fallback http.Handler) http.Handler {
	if m == nil || m.autocertManager == nil {
		return fallback
	}
	return m.autocertManager.HTTPHandler(fallback)
}

// CertificateInfo contains information about a certificate
type CertificateInfo struct {
	Domain          string
	NotBefore       time.Time
	NotAfter        time.Time
	DaysUntilExpiry int
	IsExpired       bool
	Error           error
}

// GetCertificateInfo retrieves certificate information for a domain
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

// CheckCertificates checks all configured domains and logs their certificate status
func (m *Manager) CheckCertificates() {
	if m == nil || m.autocertManager == nil {
		return
	}

	m.logger.Info("checking certificate status for all domains")

	for _, domain := range m.domains {
		info := m.GetCertificateInfo(domain)

		if info.Error != nil {
			m.logger.Warn("certificate check failed",
				zap.String("domain", domain),
				zap.Error(info.Error))
			continue
		}

		if info.IsExpired {
			m.logger.Error("certificate EXPIRED",
				zap.String("domain", domain),
				zap.Time("expired_at", info.NotAfter),
				zap.Int("days_overdue", -info.DaysUntilExpiry))
		} else if info.DaysUntilExpiry <= 7 {
			m.logger.Warn("certificate expiring soon",
				zap.String("domain", domain),
				zap.Time("expires_at", info.NotAfter),
				zap.Int("days_remaining", info.DaysUntilExpiry))
		} else if info.DaysUntilExpiry <= 30 {
			m.logger.Info("certificate status",
				zap.String("domain", domain),
				zap.Time("expires_at", info.NotAfter),
				zap.Int("days_remaining", info.DaysUntilExpiry))
		} else {
			m.logger.Debug("certificate status",
				zap.String("domain", domain),
				zap.Time("expires_at", info.NotAfter),
				zap.Int("days_remaining", info.DaysUntilExpiry))
		}
	}
}

func createS3Cache(ctx context.Context, cfg config.LetsEncryptConfig, isLeaderF func() bool, logger *zap.Logger) (*storage.S3Cache, error) {
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
	logger.Info("validating S3 bucket access", zap.String("bucket", cfg.S3.Bucket))
	_, err = s3Client.HeadBucket(initCtx, &s3.HeadBucketInput{
		Bucket: &cfg.S3.Bucket,
	})
	if err != nil {
		return nil, fmt.Errorf("S3 bucket validation failed for '%s': %w (check bucket exists and IAM permissions)", cfg.S3.Bucket, err)
	}

	logger.Info("S3 bucket validated successfully", zap.String("bucket", cfg.S3.Bucket))

	return &storage.S3Cache{
		S3Client:  s3Client,
		Bucket:    cfg.S3.Bucket,
		IsLeaderF: isLeaderF,
		Logger:    logger,
	}, nil
}
