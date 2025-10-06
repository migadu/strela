package tls

import (
	"context"
	"crypto/tls"

	"fune/internal/config"
	"fune/internal/gossip"
	"fune/internal/tls/storage"

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

func createS3Cache(cfg config.LetsEncryptConfig, isLeaderF func() bool, logger *zap.Logger) (*storage.S3Cache, error) {
	ctx := context.TODO()

	// Load default AWS config
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, err
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

	return &storage.S3Cache{
		S3Client:  s3Client,
		Bucket:    cfg.S3.Bucket,
		IsLeaderF: isLeaderF,
		Logger:    logger,
	}, nil
}
