package dkim

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"strings"
	"testing"
)

// Test helper: generate RSA key pair
func generateRSAKey(bits int) (string, error) {
	privateKey, err := rsa.GenerateKey(rand.Reader, bits)
	if err != nil {
		return "", err
	}

	privateKeyBytes := x509.MarshalPKCS1PrivateKey(privateKey)
	privateKeyPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: privateKeyBytes,
	})

	return string(privateKeyPEM), nil
}

func TestValidatePrivateKey_ValidKeys(t *testing.T) {
	tests := []struct {
		name    string
		keySize int
	}{
		{"1024-bit RSA key", 1024},
		{"2048-bit RSA key", 2048},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			keyPEM, err := generateRSAKey(tt.keySize)
			if err != nil {
				t.Fatalf("Failed to generate test key: %v", err)
			}

			size, err := ValidatePrivateKey(keyPEM)
			if err != nil {
				t.Errorf("Expected valid key, got error: %v", err)
			}

			if size != tt.keySize {
				t.Errorf("Expected key size %d, got %d", tt.keySize, size)
			}
		})
	}
}

func TestValidatePrivateKey_InvalidKeySize(t *testing.T) {
	// Test with 4096-bit key (unsupported, we only accept 1024 or 2048)
	keyPEM, err := generateRSAKey(4096)
	if err != nil {
		t.Fatalf("Failed to generate test key: %v", err)
	}

	_, err = ValidatePrivateKey(keyPEM)
	if err == nil {
		t.Error("Expected error for 4096-bit key, got nil")
	}

	if !strings.Contains(err.Error(), "unsupported RSA key size") {
		t.Errorf("Expected 'unsupported RSA key size' error, got: %v", err)
	}
}

func TestValidatePrivateKey_InvalidPEM(t *testing.T) {
	tests := []struct {
		name   string
		pemKey string
	}{
		{
			name:   "not PEM encoded",
			pemKey: "this is not a PEM key",
		},
		{
			name:   "empty string",
			pemKey: "",
		},
		{
			name:   "invalid PEM format",
			pemKey: "-----BEGIN RSA PRIVATE KEY-----\ninvalid\n-----END RSA PRIVATE KEY-----",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ValidatePrivateKey(tt.pemKey)
			if err == nil {
				t.Error("Expected error for invalid PEM, got nil")
			}
		})
	}
}

func TestValidatePrivateKey_PKCS8Format(t *testing.T) {
	// Generate key and encode as PKCS#8
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("Failed to generate test key: %v", err)
	}

	privateKeyBytes, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		t.Fatalf("Failed to marshal PKCS8 key: %v", err)
	}

	privateKeyPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "PRIVATE KEY",
		Bytes: privateKeyBytes,
	})

	size, err := ValidatePrivateKey(string(privateKeyPEM))
	if err != nil {
		t.Errorf("Expected valid PKCS#8 key, got error: %v", err)
	}

	if size != 2048 {
		t.Errorf("Expected key size 2048, got %d", size)
	}
}

func TestSignMessage_ValidKey(t *testing.T) {
	keyPEM, err := generateRSAKey(2048)
	if err != nil {
		t.Fatalf("Failed to generate test key: %v", err)
	}

	rawMessage := []byte("From: sender@example.com\r\n" +
		"To: recipient@example.com\r\n" +
		"Subject: Test\r\n" +
		"Date: Mon, 15 Jan 2025 10:00:00 +0000\r\n" +
		"Message-ID: <test@example.com>\r\n" +
		"\r\n" +
		"Test body\r\n")

	signed, err := SignMessage(rawMessage, keyPEM, "default", "example.com")
	if err != nil {
		t.Errorf("Expected successful signing, got error: %v", err)
	}

	if len(signed) == 0 {
		t.Error("Expected signed message, got empty result")
	}

	// Check DKIM signature header is present
	signedStr := string(signed)
	if !strings.Contains(signedStr, "DKIM-Signature:") {
		t.Error("Expected DKIM-Signature header in signed message")
	}

	// Verify signature parameters
	if !strings.Contains(signedStr, "d=example.com") {
		t.Error("Expected domain in DKIM signature")
	}

	if !strings.Contains(signedStr, "s=default") {
		t.Error("Expected selector in DKIM signature")
	}
}

func TestSignMessage_InvalidKey(t *testing.T) {
	rawMessage := []byte("From: sender@example.com\r\nSubject: Test\r\n\r\nBody\r\n")

	_, err := SignMessage(rawMessage, "invalid-key", "default", "example.com")
	if err == nil {
		t.Error("Expected error for invalid key, got nil")
	}
}

func TestSignMessage_UnsupportedKeySize(t *testing.T) {
	keyPEM, err := generateRSAKey(4096)
	if err != nil {
		t.Fatalf("Failed to generate test key: %v", err)
	}

	rawMessage := []byte("From: sender@example.com\r\nSubject: Test\r\n\r\nBody\r\n")

	_, err = SignMessage(rawMessage, keyPEM, "default", "example.com")
	if err == nil {
		t.Error("Expected error for 4096-bit key, got nil")
	}

	if !strings.Contains(err.Error(), "unsupported RSA key size") {
		t.Errorf("Expected 'unsupported RSA key size' error, got: %v", err)
	}
}

func TestExtractDomainFromEmail(t *testing.T) {
	tests := []struct {
		name     string
		email    string
		expected string
	}{
		{
			name:     "valid email",
			email:    "user@example.com",
			expected: "example.com",
		},
		{
			name:     "uppercase domain",
			email:    "user@EXAMPLE.COM",
			expected: "example.com",
		},
		{
			name:     "subdomain",
			email:    "user@mail.example.com",
			expected: "mail.example.com",
		},
		{
			name:     "invalid - no @",
			email:    "userexample.com",
			expected: "",
		},
		{
			name:     "invalid - multiple @",
			email:    "user@domain@example.com",
			expected: "",
		},
		{
			name:     "empty string",
			email:    "",
			expected: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := ExtractDomainFromEmail(tt.email)
			if result != tt.expected {
				t.Errorf("Expected domain '%s', got '%s'", tt.expected, result)
			}
		})
	}
}

func TestParsePrivateKey_PKCS1(t *testing.T) {
	keyPEM, err := generateRSAKey(2048)
	if err != nil {
		t.Fatalf("Failed to generate test key: %v", err)
	}

	key, err := parsePrivateKey(keyPEM)
	if err != nil {
		t.Errorf("Expected successful parsing, got error: %v", err)
	}

	if key == nil {
		t.Error("Expected valid key, got nil")
	}

	if key.N.BitLen() != 2048 {
		t.Errorf("Expected 2048-bit key, got %d bits", key.N.BitLen())
	}
}

func TestParsePrivateKey_PKCS8(t *testing.T) {
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("Failed to generate test key: %v", err)
	}

	privateKeyBytes, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		t.Fatalf("Failed to marshal PKCS8 key: %v", err)
	}

	privateKeyPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "PRIVATE KEY",
		Bytes: privateKeyBytes,
	})

	key, err := parsePrivateKey(string(privateKeyPEM))
	if err != nil {
		t.Errorf("Expected successful parsing, got error: %v", err)
	}

	if key == nil {
		t.Error("Expected valid key, got nil")
	}
}

func TestParsePrivateKey_UnsupportedType(t *testing.T) {
	// Create PEM with unsupported type
	invalidPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "CERTIFICATE",
		Bytes: []byte("fake data"),
	})

	_, err := parsePrivateKey(string(invalidPEM))
	if err == nil {
		t.Error("Expected error for unsupported PEM type, got nil")
	}

	if !strings.Contains(err.Error(), "unsupported PEM block type") {
		t.Errorf("Expected 'unsupported PEM block type' error, got: %v", err)
	}
}

func TestSignMessage_1024BitKey(t *testing.T) {
	keyPEM, err := generateRSAKey(1024)
	if err != nil {
		t.Fatalf("Failed to generate test key: %v", err)
	}

	rawMessage := []byte("From: sender@example.com\r\n" +
		"To: recipient@example.com\r\n" +
		"Subject: Test 1024\r\n" +
		"\r\n" +
		"Test body\r\n")

	signed, err := SignMessage(rawMessage, keyPEM, "mail", "example.com")
	if err != nil {
		t.Errorf("Expected successful signing with 1024-bit key, got error: %v", err)
	}

	if !strings.Contains(string(signed), "DKIM-Signature:") {
		t.Error("Expected DKIM-Signature header in signed message")
	}
}

func TestSignMessage_WithAllHeaders(t *testing.T) {
	keyPEM, err := generateRSAKey(2048)
	if err != nil {
		t.Fatalf("Failed to generate test key: %v", err)
	}

	rawMessage := []byte("From: sender@example.com\r\n" +
		"To: recipient@example.com\r\n" +
		"Subject: Full Headers Test\r\n" +
		"Date: Mon, 15 Jan 2025 10:00:00 +0000\r\n" +
		"Message-ID: <full@example.com>\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n" +
		"\r\n" +
		"Test body with all headers\r\n")

	signed, err := SignMessage(rawMessage, keyPEM, "default", "example.com")
	if err != nil {
		t.Errorf("Expected successful signing, got error: %v", err)
	}

	signedStr := string(signed)
	if !strings.Contains(signedStr, "DKIM-Signature:") {
		t.Error("Expected DKIM-Signature header")
	}

	// Original message should be preserved
	if !strings.Contains(signedStr, "Test body with all headers") {
		t.Error("Expected original message body to be preserved")
	}
}
