package tls

import (
	"context"
	"crypto/tls"
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
}

// NewManager creates a new TLS manager.
func NewManager(cfg *config.TLSConfig, gossipSvc *gossip.Gossip, logger *zap.Logger) (*Manager, error) {
	if !cfg.Enabled || cfg.Provider != "letsencrypt" {
		return nil, nil
	}

	// Validate gossip service for leader-based coordination
	if gossipSvc == nil {
		logger.Warn("gossip service not available, TLS manager will not use leader coordination")
		return nil, nil
	}

	// Create S3 cache
	s3Cache, err := createS3Cache(cfg.LetsEncrypt, gossipSvc.IsLeader, logger)
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

func createS3Cache(cfg config.LetsEncryptConfig, isLeaderF func() bool, logger *zap.Logger) (*storage.S3Cache, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Load default AWS config with retry configuration
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx,
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
	_, err = s3Client.HeadBucket(ctx, &s3.HeadBucketInput{
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
