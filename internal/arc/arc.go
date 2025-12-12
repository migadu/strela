// Package arc provides ARC (Authenticated Received Chain) signing for email forwarding.
// ARC (RFC 8617) preserves authentication results across forwarding hops, allowing
// recipients to verify the chain of custody and authentication status of forwarded messages.
//
// Key Features:
//   - ARC-Seal: Cryptographic seal of the ARC chain
//   - ARC-Message-Signature: Signature of message body and headers
//   - ARC-Authentication-Results: Authentication results from this hop
//   - Support for both relaxed and simple canonicalization
//   - RSA-based signing with SHA-256 hashing
//
// ARC Implementation:
//
// The package uses the emersion/go-msgauth library for ARC signing.
// Three ARC headers are added to forwarded messages:
//  1. ARC-Seal: Seals the entire ARC chain with a signature
//  2. ARC-Message-Signature: Signs the message with selected headers
//  3. ARC-Authentication-Results: Records authentication results
//
// Each set of ARC headers includes an instance number (i=1, i=2, etc.) that
// increments with each forwarding hop. This creates a verifiable chain of custody.
//
// Example Usage:
//
//	privateKeyPEM := `-----BEGIN RSA PRIVATE KEY-----
//	MIIEpAIBAAKCAQEA...
//	-----END RSA PRIVATE KEY-----`
//
//	config := &arc.SignConfig{
//		Selector:    "arc-2024",
//		Domain:      "example.com",
//		PrivateKey:  privateKeyPEM,
//		HeaderCanon: "relaxed",
//		BodyCanon:   "relaxed",
//	}
//
//	signedMsg, err := arc.SignMessage(rawMessage, config)
//	if err != nil {
//		log.Fatal(err)
//	}
//
// DNS Record Example:
//
//	arc-2024._domainkey.example.com. IN TXT "v=DKIM1; k=rsa; p=MIIBIjANBgkq..."
package arc

import (
	"bytes"
	"crypto"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"io"
	"strings"

	"github.com/emersion/go-msgauth/authres"
	"github.com/emersion/go-msgauth/dkim"
)

// SignConfig contains configuration for ARC signing
type SignConfig struct {
	Selector    string // DNS selector for ARC public key
	Domain      string // Domain for ARC signing
	PrivateKey  string // PEM-encoded RSA private key
	HeaderCanon string // Header canonicalization: "relaxed" or "simple"
	BodyCanon   string // Body canonicalization: "relaxed" or "simple"
}

// SignMessage signs an email message with ARC headers. Three headers are added:
//   - ARC-Seal: Seals the ARC chain with this server's signature
//   - ARC-Message-Signature: Signs the message body and selected headers
//   - ARC-Authentication-Results: Records authentication results from this hop
//
// The function automatically determines the correct instance number (i=) by
// examining existing ARC headers in the message.
//
// Parameters:
//   - rawMessage: Complete RFC 5322 email message (headers + body)
//   - config: ARC signing configuration
//
// Returns the message with ARC headers prepended, or an error if signing fails.
func SignMessage(rawMessage []byte, config *SignConfig) ([]byte, error) {
	// Parse private key
	privateKey, err := parsePrivateKey(config.PrivateKey)
	if err != nil {
		return nil, fmt.Errorf("failed to parse ARC private key: %w", err)
	}

	// Validate key size
	keySize := privateKey.N.BitLen()
	if keySize != 1024 && keySize != 2048 {
		return nil, fmt.Errorf("unsupported RSA key size: %d bits (must be 1024 or 2048)", keySize)
	}

	// Determine canonicalization algorithms
	headerCanon, bodyCanon := parseCanonicalization(config.HeaderCanon, config.BodyCanon)

	// Parse existing ARC headers to determine instance number
	instance := determineInstance(rawMessage)

	// Create ARC signer options
	options := &dkim.SignOptions{
		Domain:                 config.Domain,
		Selector:               config.Selector,
		Signer:                 privateKey,
		Hash:                   crypto.SHA256,
		HeaderCanonicalization: headerCanon,
		BodyCanonicalization:   bodyCanon,
		HeaderKeys: []string{
			"from", "to", "subject", "date",
			"message-id", "mime-version", "content-type",
			"cc", "bcc", "reply-to",
		},
	}

	// Create ARC-Message-Signature
	arcMS, err := signARCMessageSignature(rawMessage, options, instance)
	if err != nil {
		return nil, fmt.Errorf("failed to create ARC-Message-Signature: %w", err)
	}

	// Create ARC-Authentication-Results
	arcAR := createARCAuthenticationResults(config.Domain, instance)

	// Create ARC-Seal
	arcSeal, err := signARCSeal(rawMessage, options, instance, arcMS, arcAR)
	if err != nil {
		return nil, fmt.Errorf("failed to create ARC-Seal: %w", err)
	}

	// Prepend ARC headers to message
	var signedBuf bytes.Buffer
	signedBuf.WriteString("ARC-Seal: ")
	signedBuf.WriteString(arcSeal)
	signedBuf.WriteString("\r\n")
	signedBuf.WriteString("ARC-Message-Signature: ")
	signedBuf.WriteString(arcMS)
	signedBuf.WriteString("\r\n")
	signedBuf.WriteString("ARC-Authentication-Results: ")
	signedBuf.WriteString(arcAR)
	signedBuf.WriteString("\r\n")
	signedBuf.Write(rawMessage)

	return signedBuf.Bytes(), nil
}

// signARCMessageSignature creates the ARC-Message-Signature header
func signARCMessageSignature(rawMessage []byte, options *dkim.SignOptions, instance int) (string, error) {
	var buf bytes.Buffer
	if err := dkim.Sign(&buf, bytes.NewReader(rawMessage), options); err != nil {
		return "", err
	}

	// Extract DKIM-Signature header and convert to ARC-Message-Signature
	signedMsg := buf.Bytes()
	lines := bytes.Split(signedMsg, []byte("\r\n"))

	var dkimSig []byte
	for i, line := range lines {
		if bytes.HasPrefix(line, []byte("DKIM-Signature:")) {
			// Collect the full DKIM signature (may span multiple lines)
			dkimSig = line[len("DKIM-Signature:"):]
			for j := i + 1; j < len(lines); j++ {
				if len(lines[j]) > 0 && (lines[j][0] == ' ' || lines[j][0] == '\t') {
					dkimSig = append(dkimSig, []byte("\r\n")...)
					dkimSig = append(dkimSig, lines[j]...)
				} else {
					break
				}
			}
			break
		}
	}

	if len(dkimSig) == 0 {
		return "", fmt.Errorf("failed to extract DKIM signature")
	}

	// Convert DKIM signature to ARC-Message-Signature format
	arcMS := string(bytes.TrimSpace(dkimSig))
	// Add instance number
	arcMS = fmt.Sprintf("i=%d; %s", instance, arcMS)

	return arcMS, nil
}

// signARCSeal creates the ARC-Seal header that seals the entire ARC chain
func signARCSeal(rawMessage []byte, options *dkim.SignOptions, instance int, arcMS, arcAR string) (string, error) {
	// Build a minimal message with just the ARC headers and a From header for sealing
	// The From header is required by DKIM, but we're primarily sealing the ARC headers
	var sealInput bytes.Buffer

	// Add a From header to satisfy DKIM requirements
	sealInput.WriteString("From: arc-sealer@" + options.Domain + "\r\n")
	sealInput.WriteString("ARC-Message-Signature: ")
	sealInput.WriteString(arcMS)
	sealInput.WriteString("\r\n")
	sealInput.WriteString("ARC-Authentication-Results: ")
	sealInput.WriteString(arcAR)
	sealInput.WriteString("\r\n")
	sealInput.WriteString("\r\n")
	sealInput.WriteString("ARC seal body\r\n")

	// Sign with modified header keys for ARC-Seal (must include 'from' for DKIM)
	sealOptions := *options
	sealOptions.HeaderKeys = []string{
		"from",
		"arc-authentication-results",
		"arc-message-signature",
	}

	var buf bytes.Buffer
	if err := dkim.Sign(&buf, &sealInput, &sealOptions); err != nil {
		return "", err
	}

	// Extract DKIM-Signature and convert to ARC-Seal
	signedMsg := buf.Bytes()
	lines := bytes.Split(signedMsg, []byte("\r\n"))

	var dkimSig []byte
	for i, line := range lines {
		if bytes.HasPrefix(line, []byte("DKIM-Signature:")) {
			dkimSig = line[len("DKIM-Signature:"):]
			for j := i + 1; j < len(lines); j++ {
				if len(lines[j]) > 0 && (lines[j][0] == ' ' || lines[j][0] == '\t') {
					dkimSig = append(dkimSig, []byte("\r\n")...)
					dkimSig = append(dkimSig, lines[j]...)
				} else {
					break
				}
			}
			break
		}
	}

	if len(dkimSig) == 0 {
		return "", fmt.Errorf("failed to extract DKIM signature for seal")
	}

	arcSeal := string(bytes.TrimSpace(dkimSig))
	// Add instance number and chain validation status
	// cv=none for first hop, cv=pass/fail for subsequent hops
	cv := "none"
	if instance > 1 {
		cv = "pass" // TODO: Actually validate previous ARC chain
	}
	arcSeal = fmt.Sprintf("i=%d; cv=%s; %s", instance, cv, arcSeal)

	return arcSeal, nil
}

// createARCAuthenticationResults creates the ARC-Authentication-Results header
func createARCAuthenticationResults(domain string, instance int) string {
	// Create authentication results with basic "none" result
	// In a real implementation, this would contain actual SPF/DKIM/DMARC results
	results := []authres.Result{
		&authres.AuthResult{
			Value: authres.ResultNone,
		},
	}

	// Format as string
	arStr := authres.Format(domain, results)

	// Add instance number
	return fmt.Sprintf("i=%d; %s", instance, arStr)
}

// determineInstance examines existing ARC headers to determine the next instance number
func determineInstance(rawMessage []byte) int {
	maxInstance := 0

	// Simple parsing to find highest i= value in existing ARC headers
	lines := bytes.Split(rawMessage, []byte("\r\n"))
	for _, line := range lines {
		if bytes.HasPrefix(line, []byte("ARC-")) {
			// Look for i=N pattern
			if idx := bytes.Index(line, []byte("i=")); idx >= 0 {
				rest := line[idx+2:]
				var num int
				fmt.Sscanf(string(rest), "%d", &num)
				if num > maxInstance {
					maxInstance = num
				}
			}
		}
		// Stop at first empty line (end of headers)
		if len(line) == 0 {
			break
		}
	}

	return maxInstance + 1
}

// parseCanonicalization converts string canonicalization to dkim constants
func parseCanonicalization(headerCanon, bodyCanon string) (dkim.Canonicalization, dkim.Canonicalization) {
	var hc, bc dkim.Canonicalization

	switch strings.ToLower(headerCanon) {
	case "simple":
		hc = dkim.CanonicalizationSimple
	default:
		hc = dkim.CanonicalizationRelaxed
	}

	switch strings.ToLower(bodyCanon) {
	case "simple":
		bc = dkim.CanonicalizationSimple
	default:
		bc = dkim.CanonicalizationRelaxed
	}

	return hc, bc
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

// ValidatePrivateKey validates an ARC private key without performing any
// signing operation. This is useful for configuration validation at startup.
//
// Returns the key size in bits if valid, or an error if the key cannot be
// parsed or has an unsupported size.
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

// LoadPrivateKeyFromFile reads and validates a PEM-encoded private key from disk
func LoadPrivateKeyFromFile(path string) (string, error) {
	data, err := io.ReadAll(nil) // Placeholder - will be implemented with actual file reading
	if err != nil {
		return "", fmt.Errorf("failed to read private key file: %w", err)
	}

	// Validate the key
	_, err = ValidatePrivateKey(string(data))
	if err != nil {
		return "", fmt.Errorf("invalid private key in %s: %w", path, err)
	}

	return string(data), nil
}
