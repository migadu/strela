package main

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"text/tabwriter"
	"time"

	"fune/internal/config"
	"fune/internal/tls/storage"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"golang.org/x/crypto/acme/autocert"
)

// Version information, injected at build time
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

var (
	configFile  string
	showVersion bool
)

func init() {
	flag.StringVar(&configFile, "config", "config.toml", "Path to config file")
	flag.BoolVar(&showVersion, "version", false, "Show version information")
}

func main() {
	flag.Usage = usage
	flag.Parse()

	// Check for -version flag
	if showVersion {
		cmdVersion()
		os.Exit(0)
	}

	if flag.NArg() < 1 {
		usage()
		os.Exit(1)
	}

	command := flag.Arg(0)

	switch command {
	case "tls":
		cmdTLS()
	case "version", "-version", "--version":
		cmdVersion()
	default:
		fmt.Fprintf(os.Stderr, "Unknown command: %s\n\n", command)
		usage()
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, `fune-admin - Administration tool for Fune SMTP delivery gateway

Usage:
  fune-admin [flags] <command>

Commands:
  tls                Manage TLS certificates (list, delete, clean, sync)
  version            Show version information

Flags:
  -config string     Path to config file (default "config.toml")
  -version           Show version information

Examples:
  fune-admin version
  fune-admin tls list
  fune-admin tls delete example.com
  fune-admin tls clean
  fune-admin tls sync
  fune-admin -config /etc/fune/config.toml tls list

`)
}

func cmdVersion() {
	fmt.Printf("fune-admin %s\n", version)
	fmt.Printf("  commit: %s\n", commit)
	fmt.Printf("  built:  %s\n", date)
}

func cmdTLS() {
	if flag.NArg() < 2 {
		printTLSUsage()
		os.Exit(1)
	}

	subcommand := flag.Arg(1)
	ctx := context.Background()

	switch subcommand {
	case "list", "ls":
		handleTLSList(ctx)
	case "delete", "del", "rm":
		handleTLSDelete(ctx)
	case "clean":
		handleTLSClean(ctx)
	case "sync":
		handleTLSSync(ctx)
	case "help", "--help", "-h":
		printTLSUsage()
	default:
		fmt.Fprintf(os.Stderr, "Unknown tls subcommand: %s\n\n", subcommand)
		printTLSUsage()
		os.Exit(1)
	}
}

func printTLSUsage() {
	fmt.Fprintf(os.Stderr, `TLS Certificate Management

Usage:
  fune-admin tls <subcommand> [arguments]

Subcommands:
  list, ls           List all certificates in storage
  delete, del, rm    Delete certificate for a domain
  clean              Clean up expired and invalid certificates
  sync               Sync certificates from S3 to local cache (S3 storage only)
  help               Show this help message

Examples:
  fune-admin tls list
  fune-admin tls delete example.com
  fune-admin tls clean
  fune-admin tls sync

Notes:
  - Commands require a valid config.toml with TLS configuration
  - Use -config flag to specify a different config file
  - Deleted certificates will be automatically re-requested by the server
  - Storage backend (S3 or file) is determined by config.toml settings

`)
}

func handleTLSList(ctx context.Context) {
	cfg, cache := initTLSCache(ctx)

	fmt.Println("Listing certificates in storage...")
	fmt.Println()

	var certs []CertificateInfo

	// For autocert, we need to list the cache directory or S3 bucket
	// autocert stores certificates with SHA256 hash of domain name as filename
	// We'll try to list all known domains from config
	if len(cfg.TLS.LetsEncrypt.Domains) == 0 {
		fmt.Println("No domains configured in config.toml")
		return
	}

	for _, domain := range cfg.TLS.LetsEncrypt.Domains {
		// Try both ECDSA and RSA variants
		// autocert stores ECDSA as "domain" and RSA as "domain+rsa"
		for _, suffix := range []string{"", "+rsa"} {
			certKey := domain + suffix
			data, err := cache.Get(ctx, certKey)
			if err != nil {
				if err == autocert.ErrCacheMiss {
					continue // Certificate doesn't exist, skip
				}
				fmt.Printf("Warning: Failed to read certificate for %s: %v\n", domain+suffix, err)
				continue
			}

			certInfo := CertificateInfo{
				Domain:   domain,
				CacheKey: certKey,
				Size:     int64(len(data)),
			}

			if suffix == "+rsa" {
				certInfo.Domain += " (RSA)"
			}

			parseCertificateData(data, &certInfo)
			certs = append(certs, certInfo)
		}
	}

	if len(certs) == 0 {
		fmt.Println("No certificates found in storage")
		return
	}

	fmt.Printf("Found %d certificate(s)\n\n", len(certs))

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
	fmt.Fprintln(w, "DOMAIN\tTYPE\tSTATUS\tEXPIRY\tSIZE")
	fmt.Fprintln(w, "──────\t────\t──────\t──────\t────")

	for _, cert := range certs {
		status := "Valid"
		expiryStr := "-"
		if !cert.Expiry.IsZero() {
			daysUntilExpiry := int(time.Until(cert.Expiry).Hours() / 24)
			if daysUntilExpiry < 0 {
				expiryStr = "EXPIRED"
				status = "Expired"
			} else if daysUntilExpiry < 7 {
				expiryStr = fmt.Sprintf("%dd ⚠", daysUntilExpiry)
				status = "Expiring"
			} else if daysUntilExpiry < 30 {
				expiryStr = fmt.Sprintf("%dd", daysUntilExpiry)
				status = "Expiring"
			} else {
				expiryStr = fmt.Sprintf("%dd", daysUntilExpiry)
			}
		}

		if cert.CertType == "Invalid" || cert.CertType == "Parse Error" {
			status = "Invalid"
		}

		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d bytes\n",
			cert.Domain,
			cert.CertType,
			status,
			expiryStr,
			cert.Size,
		)
	}
	w.Flush()
}

func handleTLSDelete(ctx context.Context) {
	if flag.NArg() < 3 {
		fmt.Fprintln(os.Stderr, "Error: delete requires a domain argument")
		fmt.Fprintln(os.Stderr, "Usage: fune-admin tls delete <domain>")
		fmt.Fprintln(os.Stderr, "Example: fune-admin tls delete example.com")
		os.Exit(1)
	}

	domain := flag.Arg(2)
	_, cache := initTLSCache(ctx)

	fmt.Printf("Deleting certificate for domain: %s\n", domain)
	fmt.Println("Warning: This will remove the certificate from storage.")
	fmt.Println("The server will automatically request a new certificate on next use.")
	fmt.Println()

	deleted := 0
	notFound := 0
	var errors []string

	// Delete both ECDSA and RSA variants
	// autocert stores ECDSA as "domain" and RSA as "domain+rsa"
	for _, suffix := range []string{"", "+rsa"} {
		certKey := domain + suffix
		keyType := "ECDSA"
		if suffix == "+rsa" {
			keyType = "RSA"
		}

		err := cache.Delete(ctx, certKey)
		if err != nil {
			if err == autocert.ErrCacheMiss {
				fmt.Printf("  %s certificate not found (may not exist)\n", keyType)
				notFound++
			} else {
				errMsg := fmt.Sprintf("%s certificate: %v", keyType, err)
				errors = append(errors, errMsg)
				fmt.Printf("✗ Failed to delete %s\n", errMsg)
			}
		} else {
			fmt.Printf("✓ Deleted %s certificate\n", keyType)
			deleted++
		}
	}

	fmt.Println()

	if deleted > 0 {
		fmt.Printf("Successfully deleted %d certificate(s) for '%s'\n", deleted, domain)
	}
	if notFound == 2 {
		fmt.Printf("No certificates found for '%s' (may have been already deleted)\n", domain)
	}
	if len(errors) > 0 {
		fmt.Println("Errors occurred during deletion:")
		for _, e := range errors {
			fmt.Printf("  - %s\n", e)
		}
		os.Exit(1)
	}
	if deleted == 0 && notFound == 0 {
		fmt.Println("✗ No certificates deleted")
		os.Exit(1)
	}
}

func handleTLSClean(ctx context.Context) {
	cfg, cache := initTLSCache(ctx)

	fmt.Println("Cleaning up expired and invalid certificates...")
	fmt.Println()

	var toDelete []struct {
		key    string
		domain string
		reason string
	}

	// Check all configured domains
	for _, domain := range cfg.TLS.LetsEncrypt.Domains {
		for _, suffix := range []string{"", "+rsa"} {
			// autocert stores ECDSA as "domain" and RSA as "domain+rsa"
			certKey := domain + suffix
			displayDomain := domain
			if suffix == "+rsa" {
				displayDomain += " (RSA)"
			}

			data, err := cache.Get(ctx, certKey)
			if err != nil {
				if err == autocert.ErrCacheMiss {
					continue // Certificate doesn't exist
				}
				fmt.Printf("Warning: Failed to read %s: %v\n", displayDomain, err)
				continue
			}

			certInfo := CertificateInfo{
				CacheKey: certKey,
				Domain:   displayDomain,
			}
			parseCertificateData(data, &certInfo)

			// Check if expired
			if !certInfo.Expiry.IsZero() && time.Now().After(certInfo.Expiry) {
				toDelete = append(toDelete, struct {
					key    string
					domain string
					reason string
				}{certKey, displayDomain, "expired"})
			}

			// Check if invalid
			if certInfo.CertType == "Invalid" || certInfo.CertType == "Parse Error" {
				toDelete = append(toDelete, struct {
					key    string
					domain string
					reason string
				}{certKey, displayDomain, "invalid"})
			}
		}
	}

	if len(toDelete) == 0 {
		fmt.Println("No expired or invalid certificates found")
		return
	}

	fmt.Printf("Found %d certificate(s) to clean:\n\n", len(toDelete))
	for _, item := range toDelete {
		fmt.Printf("  - %s (%s)\n", item.domain, item.reason)
	}
	fmt.Println()

	deleted := 0
	failed := 0

	for _, item := range toDelete {
		err := cache.Delete(ctx, item.key)
		if err != nil {
			fmt.Printf("✗ Failed to delete %s: %v\n", item.domain, err)
			failed++
		} else {
			fmt.Printf("✓ Deleted %s (%s)\n", item.domain, item.reason)
			deleted++
		}
	}

	fmt.Println()
	fmt.Printf("Cleaned up %d certificate(s)", deleted)
	if failed > 0 {
		fmt.Printf(" (%d failed)", failed)
	}
	fmt.Println()
}

func handleTLSSync(ctx context.Context) {
	cfg, _ := initTLSCache(ctx)

	// Check if using S3 storage
	if cfg.TLS.LetsEncrypt.StorageProvider != "s3" {
		fmt.Println("Sync command only applies to S3 storage backend")
		fmt.Printf("Current storage provider: %s\n", cfg.TLS.LetsEncrypt.StorageProvider)
		os.Exit(1)
	}

	// Check if cache directory is configured
	if cfg.TLS.LetsEncrypt.CacheDir == "" {
		fmt.Println("Local cache directory not configured in config.toml")
		fmt.Println("Set tls.letsencrypt.cache_dir to enable local certificate caching")
		os.Exit(1)
	}

	fmt.Printf("Syncing certificates from S3 to local cache: %s\n", cfg.TLS.LetsEncrypt.CacheDir)
	fmt.Println()

	// Initialize S3 cache
	s3Cache, err := createS3Cache(ctx, cfg.TLS.LetsEncrypt)
	if err != nil {
		fatal("Failed to initialize S3 cache: %v", err)
	}

	// Ensure cache directory exists
	if err := os.MkdirAll(cfg.TLS.LetsEncrypt.CacheDir, 0700); err != nil {
		fatal("Failed to create cache directory: %v", err)
	}

	synced := 0
	skipped := 0
	failed := 0

	// Sync all configured domains
	for _, domain := range cfg.TLS.LetsEncrypt.Domains {
		for _, suffix := range []string{"", "+rsa"} {
			// autocert stores ECDSA as "domain" and RSA as "domain+rsa"
			certKey := domain + suffix
			localPath := filepath.Join(cfg.TLS.LetsEncrypt.CacheDir, certKey)
			displayDomain := domain
			if suffix == "+rsa" {
				displayDomain += " (RSA)"
			}

			// Download from S3
			data, err := s3Cache.Get(ctx, certKey)
			if err != nil {
				if err == autocert.ErrCacheMiss {
					skipped++
					continue // Certificate doesn't exist in S3
				}
				fmt.Printf("✗ Failed to download %s: %v\n", displayDomain, err)
				failed++
				continue
			}

			// Check if local file exists and matches
			if existingData, err := os.ReadFile(localPath); err == nil {
				if string(existingData) == string(data) {
					skipped++
					continue
				}
			}

			// Write to local cache
			if err := os.WriteFile(localPath, data, 0600); err != nil {
				fmt.Printf("✗ Failed to write %s: %v\n", displayDomain, err)
				failed++
				continue
			}

			fmt.Printf("✓ Synced %s\n", displayDomain)
			synced++
		}
	}

	fmt.Println()
	fmt.Printf("Sync complete: %d synced, %d skipped, %d failed\n", synced, skipped, failed)
}

// Helper functions

func initTLSCache(ctx context.Context) (*config.Config, autocert.Cache) {
	// Load config
	cfg, err := config.LoadConfig(configFile)
	if err != nil {
		fatal("Failed to load config: %v", err)
	}

	if !cfg.TLS.Enabled {
		fatal("TLS is not enabled in config")
	}

	if cfg.TLS.Provider != "letsencrypt" {
		fatal("TLS provider must be 'letsencrypt' (current: %s)", cfg.TLS.Provider)
	}

	var cache autocert.Cache

	switch cfg.TLS.LetsEncrypt.StorageProvider {
	case "s3":
		s3Cache, err := createS3Cache(ctx, cfg.TLS.LetsEncrypt)
		if err != nil {
			fatal("Failed to initialize S3 cache: %v", err)
		}
		cache = s3Cache

	case "file":
		if cfg.TLS.LetsEncrypt.CacheDir == "" {
			fatal("cache_dir not configured for file storage")
		}
		cache = autocert.DirCache(cfg.TLS.LetsEncrypt.CacheDir)

	default:
		fatal("Unsupported storage provider: %s", cfg.TLS.LetsEncrypt.StorageProvider)
	}

	return cfg, cache
}

func createS3Cache(ctx context.Context, cfg config.LetsEncryptConfig) (*storage.S3Cache, error) {
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to load AWS config: %w", err)
	}

	if cfg.S3.Region != "" {
		awsCfg.Region = cfg.S3.Region
	}

	if cfg.S3.AccessKey != "" && cfg.S3.SecretKey != "" {
		awsCfg.Credentials = credentials.NewStaticCredentialsProvider(
			cfg.S3.AccessKey,
			cfg.S3.SecretKey,
			"",
		)
	}

	// Configure S3 client
	var s3Client *s3.Client
	if cfg.S3.Endpoint != "" {
		s3Client = s3.NewFromConfig(awsCfg, func(o *s3.Options) {
			o.BaseEndpoint = aws.String(cfg.S3.Endpoint)
			o.UsePathStyle = true
		})
	} else {
		s3Client = s3.NewFromConfig(awsCfg)
	}

	// Create a simple logger for admin tool (writes to stderr)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelError, // Only show errors to keep output clean
	}))

	return &storage.S3Cache{
		S3Client: s3Client,
		Bucket:   cfg.S3.Bucket,
		Logger:   logger,
	}, nil
}

func hashDomain(domain string) string {
	h := sha256.Sum256([]byte(domain))
	return hex.EncodeToString(h[:])
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "Error: "+format+"\n", args...)
	os.Exit(1)
}

// Certificate information and parsing

type CertificateInfo struct {
	CacheKey  string
	Domain    string
	CertType  string // "ECDSA", "RSA", or "Unknown"
	Size      int64
	Expiry    time.Time
	NotBefore time.Time
	Subject   string
	Issuer    string
}

func parseCertificateData(data []byte, info *CertificateInfo) {
	// autocert stores: private key PEM + certificate PEM
	// Skip the private key and parse the certificate
	var certPEM []byte
	remaining := data

	for {
		block, rest := pem.Decode(remaining)
		if block == nil {
			break
		}

		if block.Type == "CERTIFICATE" {
			certPEM = pem.EncodeToMemory(block)
			break
		}

		remaining = rest
	}

	if certPEM == nil {
		info.CertType = "Invalid"
		return
	}

	// Parse certificate
	block, _ := pem.Decode(certPEM)
	if block == nil {
		info.CertType = "Invalid"
		return
	}

	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		info.CertType = "Parse Error"
		return
	}

	// Extract information
	info.Expiry = cert.NotAfter
	info.NotBefore = cert.NotBefore
	info.Subject = cert.Subject.String()
	info.Issuer = cert.Issuer.String()

	// Determine cert type from public key algorithm
	switch cert.PublicKeyAlgorithm {
	case x509.ECDSA:
		info.CertType = "ECDSA"
	case x509.RSA:
		info.CertType = "RSA"
	default:
		info.CertType = cert.PublicKeyAlgorithm.String()
	}
}
