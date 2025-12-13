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
	"fune/internal/tls/storage"

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
}

type gossipService interface{} // Placeholder or remove if unused

// NewManager creates a new TLS manager.
func NewManager(ctx context.Context, cfg *config.TLSConfig, _ gossipService, logger *slog.Logger) (*Manager, error) {
	if !cfg.Enabled || cfg.Provider != "letsencrypt" {
		return nil, nil
	}

	var cache autocert.Cache

	switch cfg.LetsEncrypt.StorageProvider {
	case "s3":
		// Create S3 cache
		s3Cache, err := createS3Cache(ctx, cfg.LetsEncrypt, logger)
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
		return nil, fmt.Errorf("unsupported storage provider: %s", cfg.LetsEncrypt.StorageProvider)
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

// TLSConfig returns a TLS configuration.
func (m *Manager) TLSConfig() *tls.Config {
	if m == nil || m.autocertManager == nil {
		return nil
	}
	return m.autocertManager.TLSConfig()
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

	if cfg.S3.AccessKeyID != "" && cfg.S3.SecretAccessKey != "" {
		awsCfg.Credentials = credentials.NewStaticCredentialsProvider(
			cfg.S3.AccessKeyID,
			cfg.S3.SecretAccessKey,
			"",
		)
	}

	s3Client := s3.NewFromConfig(awsCfg)

	logger.Info("validating S3 bucket access", "bucket", cfg.S3.Bucket)
	_, err = s3Client.HeadBucket(initCtx, &s3.HeadBucketInput{
		Bucket: &cfg.S3.Bucket,
	})
	if err != nil {
		return nil, fmt.Errorf("S3 bucket validation failed: %w", err)
	}

	logger.Info("S3 bucket validated successfully", "bucket", cfg.S3.Bucket)

	return &storage.S3Cache{
		S3Client: s3Client,
		Bucket:   cfg.S3.Bucket,
		Logger:   logger,
	}, nil
}
