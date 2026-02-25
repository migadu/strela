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
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"net"
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

	// Validate key size (1024, 2048, 3072, or 4096 bits)
	keySize := privateKey.N.BitLen()
	if keySize < 1024 || keySize > 4096 {
		return nil, fmt.Errorf("unsupported RSA key size: %d bits (must be between 1024 and 4096)", keySize)
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
	if keySize < 1024 || keySize > 4096 {
		return keySize, fmt.Errorf("unsupported RSA key size: %d bits (must be between 1024 and 4096)", keySize)
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

// ValidateDKIMConfiguration checks if a DKIM public key exists in DNS for the given
// selector and domain, and optionally verifies it matches the provided private key.
//
// Returns:
//   - nil if validation succeeds
//   - error if DNS lookup fails, public key is missing, or keys don't match
//
// This function performs two validations:
//  1. DNS lookup of selector._domainkey.domain to ensure DKIM record exists
//  2. (Optional) Verify the public key in DNS matches the provided private key
//
// Example: ValidateDKIMConfiguration(ctx, "default", "example.com", privateKeyPEM)
func ValidateDKIMConfiguration(ctx context.Context, selector, domain, privateKeyPEM string) error {
	// 1. Construct DKIM DNS record name
	dnsName := fmt.Sprintf("%s._domainkey.%s", selector, domain)

	// 2. Lookup TXT records
	resolver := &net.Resolver{}
	txtRecords, err := resolver.LookupTXT(ctx, dnsName)
	if err != nil {
		return fmt.Errorf("DKIM DNS lookup failed for %s: %w", dnsName, err)
	}

	if len(txtRecords) == 0 {
		return fmt.Errorf("no DKIM TXT record found for %s", dnsName)
	}

	// 3. Find DKIM record (starts with "v=DKIM1")
	var dkimRecord string
	// TXT records might be split across multiple strings, concatenate them
	fullRecord := strings.Join(txtRecords, "")
	if strings.Contains(fullRecord, "v=DKIM1") {
		dkimRecord = fullRecord
	}

	if dkimRecord == "" {
		return fmt.Errorf("no valid DKIM record (v=DKIM1) found for %s", dnsName)
	}

	// 4. Extract public key from DKIM record
	publicKeyB64 := extractPublicKeyFromDKIM(dkimRecord)
	if publicKeyB64 == "" {
		return fmt.Errorf("no public key (p=...) found in DKIM record for %s", dnsName)
	}

	// 5. Verify the public key matches the private key (if private key provided)
	if privateKeyPEM != "" {
		privateKey, err := parsePrivateKey(privateKeyPEM)
		if err != nil {
			return fmt.Errorf("failed to parse private key: %w", err)
		}

		// Decode the public key from DNS
		publicKeyDER, err := base64.StdEncoding.DecodeString(publicKeyB64)
		if err != nil {
			return fmt.Errorf("failed to decode public key from DNS: %w", err)
		}

		// Parse the public key
		publicKey, err := x509.ParsePKIXPublicKey(publicKeyDER)
		if err != nil {
			return fmt.Errorf("failed to parse public key from DNS: %w", err)
		}

		rsaPublicKey, ok := publicKey.(*rsa.PublicKey)
		if !ok {
			return fmt.Errorf("public key in DNS is not an RSA key")
		}

		// Compare the public keys
		if !publicKeysMatch(&privateKey.PublicKey, rsaPublicKey) {
			return fmt.Errorf("public key in DNS does not match the provided private key")
		}
	}

	return nil
}

// extractPublicKeyFromDKIM extracts the base64-encoded public key from a DKIM TXT record.
// DKIM record format: "v=DKIM1; k=rsa; p=MIIBIjANBgkq..."
func extractPublicKeyFromDKIM(record string) string {
	// Remove all whitespace (DKIM records can have spaces/newlines)
	record = strings.ReplaceAll(record, " ", "")
	record = strings.ReplaceAll(record, "\n", "")
	record = strings.ReplaceAll(record, "\t", "")

	// Split by semicolon to get key-value pairs
	parts := strings.Split(record, ";")
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if strings.HasPrefix(part, "p=") {
			return strings.TrimPrefix(part, "p=")
		}
	}

	return ""
}

// publicKeysMatch compares two RSA public keys for equality
func publicKeysMatch(key1, key2 *rsa.PublicKey) bool {
	if key1.E != key2.E {
		return false
	}
	return key1.N.Cmp(key2.N) == 0
}
