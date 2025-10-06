package tls

import (
	"context"
	"testing"

	"fune/internal/config"

	"go.uber.org/zap"
)

func TestNewManager_Disabled(t *testing.T) {
	ctx := context.Background()
	cfg := &config.TLSConfig{Enabled: false}
	manager, err := NewManager(ctx, cfg, nil, zap.NewNop())
	if err != nil {
		t.Errorf("NewManager() with disabled config should not return an error, got %v", err)
	}
	if manager != nil {
		t.Error("NewManager() with disabled config should return a nil manager")
	}
}

func TestNewManager_WrongProvider(t *testing.T) {
	ctx := context.Background()
	cfg := &config.TLSConfig{Enabled: true, Provider: "manual"}
	manager, err := NewManager(ctx, cfg, nil, zap.NewNop())
	if err != nil {
		t.Errorf("NewManager() with wrong provider should not return an error, got %v", err)
	}
	if manager != nil {
		t.Error("NewManager() with wrong provider should return a nil manager")
	}
}

func TestNewManager_NoGossip(t *testing.T) {
	ctx := context.Background()
	cfg := &config.TLSConfig{
		Enabled:  true,
		Provider: "letsencrypt",
	}
	// A nil gossip service is passed
	manager, err := NewManager(ctx, cfg, nil, zap.NewNop())
	if err != nil {
		t.Errorf("NewManager() with nil gossip service should not return an error, got %v", err)
	}
	if manager != nil {
		t.Error("NewManager() with nil gossip service should return a nil manager")
	}
}

func TestManager_TLSConfig_NilManager(t *testing.T) {
	var m *Manager
	// This should not panic
	if cfg := m.TLSConfig(); cfg != nil {
		t.Error("TLSConfig() on a nil manager should return nil")
	}
}

// Note: Tests with actual gossip service and S3 integration are skipped
// because they require a valid gossip instance and AWS credentials.
// The context parameter is tested indirectly through the S3 initialization
// path, and the S3 cache itself is thoroughly tested in s3_cache_test.go.
