package tls

import (
	"context"
	"net/http"
	"testing"

	"fune/internal/config"

	"log/slog"
)

func TestNewManager_Disabled(t *testing.T) {
	ctx := context.Background()
	cfg := &config.TLSConfig{Enabled: false}
	manager, err := NewManager(ctx, cfg, nil, slog.Default())
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
	manager, err := NewManager(ctx, cfg, nil, slog.Default())
	if err != nil {
		t.Errorf("NewManager() with wrong provider should not return an error, got %v", err)
	}
	if manager != nil {
		t.Error("NewManager() with wrong provider should return a nil manager")
	}
}

func TestNewManager_NoGossip_S3Storage(t *testing.T) {
	ctx := context.Background()
	cfg := &config.TLSConfig{
		Enabled:  true,
		Provider: "letsencrypt",
		LetsEncrypt: config.LetsEncryptConfig{
			StorageProvider: "s3",
		},
	}
	// A nil gossip service is passed with S3 storage - should return nil
	manager, err := NewManager(ctx, cfg, nil, slog.Default())
	if err != nil {
		t.Errorf("NewManager() with nil gossip service and S3 storage should not return an error, got %v", err)
	}
	if manager != nil {
		t.Error("NewManager() with nil gossip service and S3 storage should return a nil manager")
	}
}

func TestNewManager_FileStorage(t *testing.T) {
	ctx := context.Background()
	tmpDir := t.TempDir()

	cfg := &config.TLSConfig{
		Enabled:  true,
		Provider: "letsencrypt",
		LetsEncrypt: config.LetsEncryptConfig{
			Email:           "test@example.com",
			Domains:         []string{"example.com"},
			StorageProvider: "file",
			CacheDir:        tmpDir,
		},
	}

	// File storage does not require gossip service
	manager, err := NewManager(ctx, cfg, nil, slog.Default())
	if err != nil {
		t.Errorf("NewManager() with file storage should not return an error, got %v", err)
	}
	if manager == nil {
		t.Fatal("NewManager() with file storage should return a valid manager")
	}

	// Verify manager was initialized correctly
	if manager.autocertManager == nil {
		t.Error("autocertManager should be initialized")
	}
	if len(manager.domains) != 1 || manager.domains[0] != "example.com" {
		t.Errorf("expected domains [example.com], got %v", manager.domains)
	}
}

func TestNewManager_InvalidStorageProvider(t *testing.T) {
	ctx := context.Background()
	cfg := &config.TLSConfig{
		Enabled:  true,
		Provider: "letsencrypt",
		LetsEncrypt: config.LetsEncryptConfig{
			Email:           "test@example.com",
			Domains:         []string{"example.com"},
			StorageProvider: "invalid",
		},
	}

	manager, err := NewManager(ctx, cfg, nil, slog.Default())
	if err == nil {
		t.Error("NewManager() with invalid storage provider should return an error")
	}
	if manager != nil {
		t.Error("NewManager() with invalid storage provider should return a nil manager")
	}

	expectedErrMsg := "unsupported storage provider: invalid (must be 's3' or 'file')"
	if err.Error() != expectedErrMsg {
		t.Errorf("expected error message '%s', got '%s'", expectedErrMsg, err.Error())
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

func TestManager_HTTPHandler_NilManager(t *testing.T) {
	var m *Manager
	fallback := &testHandler{name: "fallback"}

	handler := m.HTTPHandler(fallback)

	if handler != fallback {
		t.Error("HTTPHandler() on a nil manager should return the fallback handler")
	}
}

func TestManager_HTTPHandler_ValidManager(t *testing.T) {
	// Create a manager with autocert manager set (but not fully initialized)
	m := &Manager{
		autocertManager: nil, // Even with nil autocert, should return fallback
		logger:          slog.Default(),
	}

	fallback := &testHandler{name: "fallback"}
	handler := m.HTTPHandler(fallback)

	if handler != fallback {
		t.Error("HTTPHandler() with nil autocertManager should return the fallback handler")
	}
}

func TestManager_GetCertificateInfo_NilManager(t *testing.T) {
	var m *Manager

	info := m.GetCertificateInfo("example.com")

	if info.Error == nil {
		t.Error("GetCertificateInfo() on a nil manager should return an error")
	}

	if info.Domain != "example.com" {
		t.Errorf("expected domain 'example.com', got '%s'", info.Domain)
	}
}

func TestManager_GetCertificateInfo_NilAutocertManager(t *testing.T) {
	m := &Manager{
		autocertManager: nil,
		logger:          slog.Default(),
	}

	info := m.GetCertificateInfo("example.com")

	if info.Error == nil {
		t.Error("GetCertificateInfo() with nil autocertManager should return an error")
	}

	expectedMsg := "TLS manager not initialized"
	if info.Error.Error() != expectedMsg {
		t.Errorf("expected error message '%s', got '%s'", expectedMsg, info.Error.Error())
	}
}

func TestManager_CheckCertificates_NilManager(t *testing.T) {
	var m *Manager

	// Should not panic
	m.CheckCertificates()
}

func TestManager_CheckCertificates_NilAutocertManager(t *testing.T) {
	m := &Manager{
		autocertManager: nil,
		logger:          slog.Default(),
		domains:         []string{"example.com"},
	}

	// Should not panic
	m.CheckCertificates()
}

// testHandler is a simple HTTP handler for testing
type testHandler struct {
	name string
}

func (h *testHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Write([]byte(h.name))
}
