package dkim

import (
	"bytes"
	"crypto"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"strings"
	"time"

	"github.com/emersion/go-msgauth/dkim"
)

// SignMessage signs an email message with DKIM
// Supports both 1024-bit and 2048-bit RSA keys
func SignMessage(rawMessage []byte, privateKeyPEM, selector, domain string) ([]byte, error) {
	// Parse private key
	privateKey, err := parsePrivateKey(privateKeyPEM)
	if err != nil {
		return nil, fmt.Errorf("failed to parse DKIM private key: %w", err)
	}

	// Validate key size (1024 or 2048 bits)
	keySize := privateKey.N.BitLen()
	if keySize != 1024 && keySize != 2048 {
		return nil, fmt.Errorf("unsupported RSA key size: %d bits (must be 1024 or 2048)", keySize)
	}

	// Set up DKIM options
	options := &dkim.SignOptions{
		Domain:   domain,
		Selector: selector,
		Signer:   privateKey,
		Hash:     crypto.SHA256,
		HeaderKeys: []string{
			"from", "to", "subject", "date",
			"message-id", "mime-version", "content-type",
		},
		Expiration: time.Now().Add(7 * 24 * time.Hour), // 7 days
	}

	// Sign the message
	var signedBuf bytes.Buffer
	err = dkim.Sign(&signedBuf, bytes.NewReader(rawMessage), options)
	if err != nil {
		return nil, fmt.Errorf("failed to sign message with DKIM: %w", err)
	}

	return signedBuf.Bytes(), nil
}

// parsePrivateKey parses a PEM-encoded RSA private key
func parsePrivateKey(pemData string) (*rsa.PrivateKey, error) {
	pemData = strings.TrimSpace(pemData)

	// Decode PEM block
	block, _ := pem.Decode([]byte(pemData))
	if block == nil {
		return nil, fmt.Errorf("failed to decode PEM block")
	}

	// Try parsing as PKCS#1 RSA private key
	if block.Type == "RSA PRIVATE KEY" {
		key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("failed to parse PKCS#1 RSA private key: %w", err)
		}
		return key, nil
	}

	// Try parsing as PKCS#8 private key
	if block.Type == "PRIVATE KEY" {
		parsedKey, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("failed to parse PKCS#8 private key: %w", err)
		}

		rsaKey, ok := parsedKey.(*rsa.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("parsed key is not an RSA private key")
		}
		return rsaKey, nil
	}

	return nil, fmt.Errorf("unsupported PEM block type: %s (expected 'RSA PRIVATE KEY' or 'PRIVATE KEY')", block.Type)
}

// ValidatePrivateKey validates a DKIM private key without signing
// Returns the key size in bits or an error
func ValidatePrivateKey(privateKeyPEM string) (int, error) {
	privateKey, err := parsePrivateKey(privateKeyPEM)
	if err != nil {
		return 0, err
	}

	keySize := privateKey.N.BitLen()
	if keySize != 1024 && keySize != 2048 {
		return keySize, fmt.Errorf("unsupported RSA key size: %d bits (must be 1024 or 2048)", keySize)
	}

	return keySize, nil
}

// ExtractDomainFromEmail extracts the domain part from an email address
func ExtractDomainFromEmail(email string) string {
	parts := strings.Split(email, "@")
	if len(parts) != 2 {
		return ""
	}
	return strings.ToLower(parts[1])
}
