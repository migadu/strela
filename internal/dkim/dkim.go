// Package dkim provides DKIM (DomainKeys Identified Mail) email signing capabilities.
// DKIM adds cryptographic signatures to email messages, allowing recipients to verify
// that messages were authorized by the domain owner and not modified in transit.
//
// Key Features:
//   - RSA-based DKIM signing with SHA-256 hashing
//   - Support for 1024-bit and 2048-bit RSA keys
//   - PKCS#1 and PKCS#8 private key formats
//   - Configurable signature expiration (default: 7 days)
//   - Standard header field signing (From, To, Subject, Date, etc.)
//
// DKIM Implementation:
//
// The package uses the emersion/go-msgauth library for DKIM signature generation.
// Signatures are prepended to the original message as a DKIM-Signature header.
// Recipients can verify signatures using the public key published in the domain's
// DNS TXT records (typically at selector._domainkey.example.com).
//
// Example Usage:
//
//	privateKeyPEM := `-----BEGIN RSA PRIVATE KEY-----
//	MIIEpAIBAAKCAQEA...
//	-----END RSA PRIVATE KEY-----`
//
//	signedMsg, err := dkim.SignMessage(rawMessage, privateKeyPEM, "default", "example.com")
//	if err != nil {
//		log.Fatal(err)
//	}
//
// DNS Record Example:
//
//	default._domainkey.example.com. IN TXT "v=DKIM1; k=rsa; p=MIIBIjANBgkq..."
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

// SignMessage signs an email message with DKIM using RSA-SHA256. The signature
// is prepended to the raw message as a DKIM-Signature header field. Supports
// both 1024-bit and 2048-bit RSA private keys in PKCS#1 or PKCS#8 PEM format.
//
// Parameters:
//   - rawMessage: Complete RFC 5322 email message (headers + body)
//   - privateKeyPEM: PEM-encoded RSA private key (1024 or 2048 bits)
//   - selector: DKIM selector (corresponds to DNS TXT record at selector._domainkey.domain)
//   - domain: Signing domain (must match From header domain)
//
// Returns the signed message with DKIM-Signature header prepended, or an error
// if signing fails. The signature expires after 7 days by default.
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

// ValidatePrivateKey validates a DKIM private key without performing any
// signing operation. This is useful for configuration validation at startup
// to catch key format issues early.
//
// Returns the key size in bits (1024 or 2048) if valid, or an error if the
// key cannot be parsed or has an unsupported size. Both PKCS#1 and PKCS#8
// formats are supported.
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
// (the part after the @ symbol). Returns empty string if the email address
// is malformed or does not contain exactly one @ symbol.
//
// Example: ExtractDomainFromEmail("user@example.com") returns "example.com"
func ExtractDomainFromEmail(email string) string {
	parts := strings.Split(email, "@")
	if len(parts) != 2 {
		return ""
	}
	return strings.ToLower(parts[1])
}
