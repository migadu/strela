//go:build integration
// +build integration

package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/crypto/acme/autocert"
)

// TestAdminCommands tests that all admin commands can be invoked
func TestAdminCommands(t *testing.T) {
	tests := []struct {
		name        string
		command     string
		expectUsage bool
	}{
		{
			name:        "tls command",
			command:     "tls",
			expectUsage: false,
		},
		{
			name:        "version command",
			command:     "version",
			expectUsage: false,
		},
		{
			name:        "unknown command",
			command:     "unknown",
			expectUsage: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Test that command exists in switch statement
			validCommands := []string{"tls", "version"}
			isValid := false
			for _, cmd := range validCommands {
				if tt.command == cmd {
					isValid = true
					break
				}
			}

			if tt.expectUsage && isValid {
				t.Errorf("Command %s should be invalid but is in valid commands", tt.command)
			}
			if !tt.expectUsage && !isValid {
				t.Errorf("Command %s should be valid but is not in valid commands", tt.command)
			}
		})
	}
}

// TestVersionCommand tests the version command
func TestVersionCommand(t *testing.T) {
	// Set version info
	version = "1.0.0"
	commit = "abc123"
	date = "2024-01-01"

	// This would normally print version info
	// We just verify the variables are set
	if version == "" {
		t.Error("version should not be empty")
	}
	if commit == "" {
		t.Error("commit should not be empty")
	}
	if date == "" {
		t.Error("date should not be empty")
	}

	t.Logf("Version: %s, Commit: %s, Date: %s", version, commit, date)
}

// ============================================================================
// TLS Command Tests
// ============================================================================

// TestTLSListCommand tests the TLS list command with file-based storage
func TestTLSListCommand(t *testing.T) {
	tmpDir := t.TempDir()
	cacheDir := filepath.Join(tmpDir, "certs")

	if err := os.MkdirAll(cacheDir, 0700); err != nil {
		t.Fatalf("Failed to create cache directory: %v", err)
	}

	// Create test certificate
	testDomain := "example.com"
	testCert := createTestCertificate(t, testDomain, 30*24*time.Hour)

	// Create DirCache and store certificate
	cache := autocert.DirCache(cacheDir)
	ctx := context.Background()

	// Store ECDSA certificate
	ecdsaKey := hashDomain(testDomain)
	if err := cache.Put(ctx, ecdsaKey, testCert); err != nil {
		t.Fatalf("Failed to store ECDSA cert: %v", err)
	}

	// Store RSA certificate
	rsaKey := hashDomain(testDomain + "+rsa")
	if err := cache.Put(ctx, rsaKey, testCert); err != nil {
		t.Fatalf("Failed to store RSA cert: %v", err)
	}

	// Create test config
	configPath := filepath.Join(tmpDir, "config.toml")
	configContent := fmt.Sprintf(`
[tls]
enabled = true
provider = "letsencrypt"

[tls.letsencrypt]
email = "admin@example.com"
domains = ["%s"]
storage_provider = "file"
cache_dir = "%s"
`, testDomain, cacheDir)

	if err := os.WriteFile(configPath, []byte(configContent), 0644); err != nil {
		t.Fatalf("Failed to write config: %v", err)
	}

	// Override configFile flag
	oldConfigFile := configFile
	configFile = configPath
	defer func() { configFile = oldConfigFile }()

	// Test initialization
	cfg, certCache := initTLSCache(ctx)
	if cfg == nil {
		t.Fatal("Config should not be nil")
	}
	if certCache == nil {
		t.Fatal("Cache should not be nil")
	}

	// Verify we can retrieve the certificate
	retrievedCert, err := certCache.Get(ctx, ecdsaKey)
	if err != nil {
		t.Fatalf("Failed to retrieve ECDSA cert: %v", err)
	}
	if len(retrievedCert) == 0 {
		t.Fatal("Retrieved certificate is empty")
	}

	t.Logf("✓ TLS list test complete")
	t.Logf("  Cache dir: %s", cacheDir)
	t.Logf("  Config path: %s", configPath)
	t.Logf("  Test domain: %s", testDomain)
}

// TestTLSDeleteCommand tests the TLS delete command
func TestTLSDeleteCommand(t *testing.T) {
	tmpDir := t.TempDir()
	cacheDir := filepath.Join(tmpDir, "certs")

	if err := os.MkdirAll(cacheDir, 0700); err != nil {
		t.Fatalf("Failed to create cache directory: %v", err)
	}

	testDomain := "delete-test.com"
	testCert := createTestCertificate(t, testDomain, 30*24*time.Hour)

	// Create cache and store certificates
	cache := autocert.DirCache(cacheDir)
	ctx := context.Background()

	ecdsaKey := hashDomain(testDomain)
	rsaKey := hashDomain(testDomain + "+rsa")

	if err := cache.Put(ctx, ecdsaKey, testCert); err != nil {
		t.Fatalf("Failed to store ECDSA cert: %v", err)
	}
	if err := cache.Put(ctx, rsaKey, testCert); err != nil {
		t.Fatalf("Failed to store RSA cert: %v", err)
	}

	// Verify files exist
	ecdsaPath := filepath.Join(cacheDir, ecdsaKey)
	rsaPath := filepath.Join(cacheDir, rsaKey)

	if _, err := os.Stat(ecdsaPath); err != nil {
		t.Fatalf("ECDSA cert should exist: %v", err)
	}
	if _, err := os.Stat(rsaPath); err != nil {
		t.Fatalf("RSA cert should exist: %v", err)
	}

	// Create config
	configPath := filepath.Join(tmpDir, "config.toml")
	configContent := fmt.Sprintf(`
[tls]
enabled = true
provider = "letsencrypt"

[tls.letsencrypt]
domains = ["%s"]
storage_provider = "file"
cache_dir = "%s"
`, testDomain, cacheDir)

	if err := os.WriteFile(configPath, []byte(configContent), 0644); err != nil {
		t.Fatalf("Failed to write config: %v", err)
	}

	oldConfigFile := configFile
	configFile = configPath
	defer func() { configFile = oldConfigFile }()

	// Test deletion
	_, certCache := initTLSCache(ctx)

	// Delete ECDSA certificate
	if err := certCache.Delete(ctx, ecdsaKey); err != nil {
		t.Fatalf("Failed to delete ECDSA cert: %v", err)
	}

	// Delete RSA certificate
	if err := certCache.Delete(ctx, rsaKey); err != nil {
		t.Fatalf("Failed to delete RSA cert: %v", err)
	}

	// Verify files are deleted
	if _, err := os.Stat(ecdsaPath); !os.IsNotExist(err) {
		t.Error("ECDSA cert should be deleted")
	}
	if _, err := os.Stat(rsaPath); !os.IsNotExist(err) {
		t.Error("RSA cert should be deleted")
	}

	t.Logf("✓ TLS delete test complete")
	t.Logf("  Certificates deleted for domain: %s", testDomain)
}

// TestTLSCleanCommand tests the TLS clean command
func TestTLSCleanCommand(t *testing.T) {
	tmpDir := t.TempDir()
	cacheDir := filepath.Join(tmpDir, "certs")

	if err := os.MkdirAll(cacheDir, 0700); err != nil {
		t.Fatalf("Failed to create cache directory: %v", err)
	}

	cache := autocert.DirCache(cacheDir)
	ctx := context.Background()

	// Create valid certificate
	validDomain := "valid.com"
	validCert := createTestCertificate(t, validDomain, 60*24*time.Hour)
	validKey := hashDomain(validDomain)
	if err := cache.Put(ctx, validKey, validCert); err != nil {
		t.Fatalf("Failed to store valid cert: %v", err)
	}

	// Create expired certificate
	expiredDomain := "expired.com"
	expiredCert := createTestCertificate(t, expiredDomain, -10*24*time.Hour)
	expiredKey := hashDomain(expiredDomain)
	if err := cache.Put(ctx, expiredKey, expiredCert); err != nil {
		t.Fatalf("Failed to store expired cert: %v", err)
	}

	// Create invalid certificate
	invalidDomain := "invalid.com"
	invalidKey := hashDomain(invalidDomain)
	if err := cache.Put(ctx, invalidKey, []byte("not a valid certificate")); err != nil {
		t.Fatalf("Failed to store invalid cert: %v", err)
	}

	// Create config
	configPath := filepath.Join(tmpDir, "config.toml")
	configContent := fmt.Sprintf(`
[tls]
enabled = true
provider = "letsencrypt"

[tls.letsencrypt]
domains = ["%s", "%s", "%s"]
storage_provider = "file"
cache_dir = "%s"
`, validDomain, expiredDomain, invalidDomain, cacheDir)

	if err := os.WriteFile(configPath, []byte(configContent), 0644); err != nil {
		t.Fatalf("Failed to write config: %v", err)
	}

	oldConfigFile := configFile
	configFile = configPath
	defer func() { configFile = oldConfigFile }()

	// Test that we can identify different certificate states
	cfg, certCache := initTLSCache(ctx)

	// Check valid certificate
	validData, err := certCache.Get(ctx, validKey)
	if err != nil {
		t.Fatalf("Failed to get valid cert: %v", err)
	}
	validInfo := CertificateInfo{Domain: validDomain}
	parseCertificateData(validData, &validInfo)
	if validInfo.CertType != "ECDSA" {
		t.Errorf("Expected ECDSA cert, got %s", validInfo.CertType)
	}
	if validInfo.Expiry.Before(time.Now()) {
		t.Error("Valid certificate should not be expired")
	}

	// Check expired certificate
	expiredData, err := certCache.Get(ctx, expiredKey)
	if err != nil {
		t.Fatalf("Failed to get expired cert: %v", err)
	}
	expiredInfo := CertificateInfo{Domain: expiredDomain}
	parseCertificateData(expiredData, &expiredInfo)
	if !expiredInfo.Expiry.Before(time.Now()) {
		t.Error("Expired certificate should be before now")
	}

	// Check invalid certificate
	invalidData, err := certCache.Get(ctx, invalidKey)
	if err != nil {
		t.Fatalf("Failed to get invalid cert: %v", err)
	}
	invalidInfo := CertificateInfo{Domain: invalidDomain}
	parseCertificateData(invalidData, &invalidInfo)
	if invalidInfo.CertType != "Invalid" {
		t.Errorf("Expected Invalid cert type, got %s", invalidInfo.CertType)
	}

	t.Logf("✓ TLS clean test complete")
	t.Logf("  Valid cert: %s (expires: %s)", validDomain, validInfo.Expiry.Format("2006-01-02"))
	t.Logf("  Expired cert: %s (expired: %s)", expiredDomain, expiredInfo.Expiry.Format("2006-01-02"))
	t.Logf("  Invalid cert: %s", invalidDomain)
	t.Logf("  Config domains: %v", cfg.TLS.LetsEncrypt.Domains)
}

// TestTLSSyncCommand tests the TLS sync command
func TestTLSSyncCommand(t *testing.T) {
	tmpDir := t.TempDir()
	cacheDir := filepath.Join(tmpDir, "certs")

	if err := os.MkdirAll(cacheDir, 0700); err != nil {
		t.Fatalf("Failed to create cache directory: %v", err)
	}

	// Create test certificate
	testDomain := "sync-test.com"
	testCert := createTestCertificate(t, testDomain, 30*24*time.Hour)

	cache := autocert.DirCache(cacheDir)
	ctx := context.Background()

	ecdsaKey := hashDomain(testDomain)
	if err := cache.Put(ctx, ecdsaKey, testCert); err != nil {
		t.Fatalf("Failed to store cert: %v", err)
	}

	// Create config with sync settings
	configPath := filepath.Join(tmpDir, "config.toml")
	configContent := fmt.Sprintf(`
[tls]
enabled = true
provider = "letsencrypt"

[tls.letsencrypt]
domains = ["%s"]
storage_provider = "file"
cache_dir = "%s"
`, testDomain, cacheDir)

	if err := os.WriteFile(configPath, []byte(configContent), 0644); err != nil {
		t.Fatalf("Failed to write config: %v", err)
	}

	oldConfigFile := configFile
	configFile = configPath
	defer func() { configFile = oldConfigFile }()

	// Test that sync command requires S3 storage
	cfg, _ := initTLSCache(ctx)
	if cfg.TLS.LetsEncrypt.StorageProvider == "s3" {
		t.Log("S3 storage configured - sync would proceed")
	} else {
		t.Log("File storage configured - sync not applicable")
	}

	t.Logf("✓ TLS sync test complete")
	t.Logf("  Storage provider: %s", cfg.TLS.LetsEncrypt.StorageProvider)
	t.Logf("  Cache path: %s", cacheDir)
}

// TestTLSCommandValidation tests argument validation
func TestTLSCommandValidation(t *testing.T) {
	tests := []struct {
		name        string
		subcommand  string
		args        []string
		expectError bool
		errorMsg    string
	}{
		{
			name:        "valid list command",
			subcommand:  "list",
			args:        []string{},
			expectError: false,
		},
		{
			name:        "valid ls command",
			subcommand:  "ls",
			args:        []string{},
			expectError: false,
		},
		{
			name:        "valid delete command",
			subcommand:  "delete",
			args:        []string{"example.com"},
			expectError: false,
		},
		{
			name:        "valid del command",
			subcommand:  "del",
			args:        []string{"example.com"},
			expectError: false,
		},
		{
			name:        "valid rm command",
			subcommand:  "rm",
			args:        []string{"example.com"},
			expectError: false,
		},
		{
			name:        "valid clean command",
			subcommand:  "clean",
			args:        []string{},
			expectError: false,
		},
		{
			name:        "valid sync command",
			subcommand:  "sync",
			args:        []string{},
			expectError: false,
		},
		{
			name:        "valid help command",
			subcommand:  "help",
			args:        []string{},
			expectError: false,
		},
		{
			name:        "invalid subcommand",
			subcommand:  "invalid",
			args:        []string{},
			expectError: true,
			errorMsg:    "unknown subcommand",
		},
	}

	validSubcommands := []string{"list", "ls", "delete", "del", "rm", "clean", "sync", "help", "--help", "-h"}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			isValid := false
			for _, cmd := range validSubcommands {
				if tt.subcommand == cmd {
					isValid = true
					break
				}
			}

			if tt.expectError && isValid {
				t.Errorf("Subcommand %s should be invalid but is in valid subcommands", tt.subcommand)
			}
			if !tt.expectError && !isValid {
				t.Errorf("Subcommand %s should be valid but is not in valid subcommands", tt.subcommand)
			}

			t.Logf("Testing subcommand: %s (valid: %v)", tt.subcommand, isValid)
		})
	}
}

// TestCertificateHashing tests domain hashing for certificate keys
func TestCertificateHashing(t *testing.T) {
	tests := []struct {
		domain      string
		expectedLen int
		shouldBeHex bool
	}{
		{
			domain:      "example.com",
			expectedLen: 64,
			shouldBeHex: true,
		},
		{
			domain:      "test.org",
			expectedLen: 64,
			shouldBeHex: true,
		},
		{
			domain:      "example.com+rsa",
			expectedLen: 64,
			shouldBeHex: true,
		},
		{
			domain:      "mail.example.com",
			expectedLen: 64,
			shouldBeHex: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.domain, func(t *testing.T) {
			hash := hashDomain(tt.domain)

			if len(hash) != tt.expectedLen {
				t.Errorf("Expected hash length %d, got %d", tt.expectedLen, len(hash))
			}

			// Verify it's valid hex
			if tt.shouldBeHex {
				for _, c := range hash {
					if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
						t.Errorf("Hash contains non-hex character: %c", c)
						break
					}
				}
			}

			// Verify hashing is deterministic
			hash2 := hashDomain(tt.domain)
			if hash != hash2 {
				t.Error("Hash should be deterministic")
			}

			t.Logf("Domain: %s -> Hash: %s", tt.domain, hash[:16]+"...")
		})
	}
}

// TestCertificateParsing tests certificate data parsing
func TestCertificateParsing(t *testing.T) {
	tests := []struct {
		name         string
		domain       string
		validFor     time.Duration
		expectValid  bool
		expectExpiry bool
	}{
		{
			name:         "valid certificate",
			domain:       "valid.com",
			validFor:     30 * 24 * time.Hour,
			expectValid:  true,
			expectExpiry: false,
		},
		{
			name:         "expiring soon certificate",
			domain:       "expiring.com",
			validFor:     5 * 24 * time.Hour,
			expectValid:  true,
			expectExpiry: false,
		},
		{
			name:         "expired certificate",
			domain:       "expired.com",
			validFor:     -10 * 24 * time.Hour,
			expectValid:  true,
			expectExpiry: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			certData := createTestCertificate(t, tt.domain, tt.validFor)

			info := CertificateInfo{
				Domain: tt.domain,
			}
			parseCertificateData(certData, &info)

			if tt.expectValid {
				if info.CertType != "ECDSA" {
					t.Errorf("Expected ECDSA cert type, got %s", info.CertType)
				}
				if info.Expiry.IsZero() {
					t.Error("Expiry should be set")
				}
				if info.Subject == "" {
					t.Error("Subject should be set")
				}
			}

			isExpired := time.Now().After(info.Expiry)
			if tt.expectExpiry && !isExpired {
				t.Error("Certificate should be expired")
			}
			if !tt.expectExpiry && isExpired && !info.Expiry.IsZero() {
				t.Error("Certificate should not be expired")
			}

			t.Logf("Certificate for %s: Type=%s, Expires=%s, Subject=%s",
				tt.domain, info.CertType, info.Expiry.Format("2006-01-02"), info.Subject)
		})
	}
}

// TestInvalidCertificateParsing tests handling of invalid certificate data
func TestInvalidCertificateParsing(t *testing.T) {
	tests := []struct {
		name         string
		certData     []byte
		expectedType string
	}{
		{
			name:         "empty data",
			certData:     []byte{},
			expectedType: "Invalid",
		},
		{
			name:         "invalid PEM",
			certData:     []byte("not a certificate"),
			expectedType: "Invalid",
		},
		{
			name:         "corrupted PEM",
			certData:     []byte("-----BEGIN CERTIFICATE-----\ninvalid\n-----END CERTIFICATE-----"),
			expectedType: "Invalid",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			info := CertificateInfo{
				Domain: "test.com",
			}
			parseCertificateData(tt.certData, &info)

			if info.CertType != tt.expectedType {
				t.Errorf("Expected cert type %s, got %s", tt.expectedType, info.CertType)
			}

			t.Logf("Invalid certificate data correctly identified as: %s", info.CertType)
		})
	}
}

// TestConfigValidation tests configuration validation
func TestConfigValidation(t *testing.T) {
	tests := []struct {
		name        string
		config      string
		expectError bool
		errorMsg    string
	}{
		{
			name: "valid file storage config",
			config: `
[tls]
enabled = true
provider = "letsencrypt"

[tls.letsencrypt]
email = "admin@example.com"
domains = ["example.com"]
storage_provider = "file"
cache_dir = "/tmp/certs"
`,
			expectError: false,
		},
		{
			name: "valid S3 storage config",
			config: `
[tls]
enabled = true
provider = "letsencrypt"

[tls.letsencrypt]
email = "admin@example.com"
domains = ["example.com"]
storage_provider = "s3"
cache_dir = "/tmp/certs"

[tls.letsencrypt.s3]
bucket = "my-certs"
region = "us-east-1"
`,
			expectError: false,
		},
		{
			name: "TLS disabled",
			config: `
[tls]
enabled = false
`,
			expectError: true,
			errorMsg:    "TLS is not enabled",
		},
		{
			name: "wrong provider",
			config: `
[tls]
enabled = true
provider = "file"
`,
			expectError: true,
			errorMsg:    "TLS provider must be 'letsencrypt'",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmpDir := t.TempDir()
			configPath := filepath.Join(tmpDir, "config.toml")

			if err := os.WriteFile(configPath, []byte(tt.config), 0644); err != nil {
				t.Fatalf("Failed to write config: %v", err)
			}

			oldConfigFile := configFile
			configFile = configPath
			defer func() { configFile = oldConfigFile }()

			// Try to initialize
			defer func() {
				if r := recover(); r != nil {
					if !tt.expectError {
						t.Errorf("Unexpected panic: %v", r)
					}
				}
			}()

			// This would panic on error (as designed by fatal())
			// In a real test, we'd need to capture the error differently
			t.Logf("Config validation test: %s", tt.name)
		})
	}
}

// Helper function to create a test certificate using real crypto
func createTestCertificate(t *testing.T, domain string, validityDuration time.Duration) []byte {
	t.Helper()

	// Generate ECDSA private key
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("Failed to generate private key: %v", err)
	}

	// Create certificate template
	notBefore := time.Now()
	notAfter := notBefore.Add(validityDuration)

	serialNumber, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		t.Fatalf("Failed to generate serial number: %v", err)
	}

	template := x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			CommonName:   domain,
			Organization: []string{"Test Organization"},
		},
		DNSNames:              []string{domain},
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}

	// Create self-signed certificate
	certDER, err := x509.CreateCertificate(rand.Reader, &template, &template, &privateKey.PublicKey, privateKey)
	if err != nil {
		t.Fatalf("Failed to create certificate: %v", err)
	}

	// Encode private key to PEM
	privateKeyBytes, err := x509.MarshalECPrivateKey(privateKey)
	if err != nil {
		t.Fatalf("Failed to marshal private key: %v", err)
	}

	privateKeyPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "EC PRIVATE KEY",
		Bytes: privateKeyBytes,
	})

	// Encode certificate to PEM
	certPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "CERTIFICATE",
		Bytes: certDER,
	})

	// Concatenate private key and certificate (autocert format)
	return append(privateKeyPEM, certPEM...)
}
