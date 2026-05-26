package delivery

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"hash/fnv"
	"io"
	"log/slog"
	"math/rand"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"strela/internal/arc"
	"strela/internal/config"
	"strela/internal/dkim"
	"strela/internal/recovery"
	"strela/internal/srs"

	"github.com/emersion/go-smtp"
)

// ErrDomainRateLimitExceeded is returned when the domain rate limit is exceeded (Fail Fast).
var ErrDomainRateLimitExceeded = fmt.Errorf("domain rate limit exceeded")

var opportunisticCipherSuites []uint16

func init() {
	for _, c := range tls.CipherSuites() {
		opportunisticCipherSuites = append(opportunisticCipherSuites, c.ID)
	}
	for _, c := range tls.InsecureCipherSuites() {
		opportunisticCipherSuites = append(opportunisticCipherSuites, c.ID)
	}
}

// contextKey is an unexported type for context keys in this package.
type contextKey string

const traceIDKey contextKey = "trace_id"

// WithTraceID returns a new context carrying the given trace ID.
func WithTraceID(ctx context.Context, traceID string) context.Context {
	return context.WithValue(ctx, traceIDKey, traceID)
}

// TraceIDFromContext retrieves the trace ID from the context, or empty string if not set.
func TraceIDFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(traceIDKey).(string); ok {
		return v
	}
	return ""
}

// DeliveryResult contains the complete result of a delivery attempt.
type DeliveryResult struct {
	TraceID           string `json:"trace_id"`            // Unique trace ID for this delivery session
	Status            string `json:"status"`              // "delivered", "temp_fail", "hard_bounce", "timeout", "error"
	SMTPCode          int    `json:"smtp_code"`           // SMTP response code or 0
	SMTPMessage       string `json:"smtp_message"`        // SMTP response text
	MXHost            string `json:"mx_host"`             // MX server hostname
	SourceIP          string `json:"source_ip"`           // Source IP used
	AttemptDurationMs int64  `json:"attempt_duration_ms"` // Delivery duration
	Error             string `json:"error,omitempty"`     // Error details
}

// Deliverer is the main delivery engine that handles direct SMTP delivery.
type Deliverer struct {
	configMu          sync.RWMutex // protects config
	config            *config.OutboundConfig
	arcConfig         *config.ARCConfig
	mxLookup          *MXLookup
	logger            *slog.Logger
	ipRotator         *IPRotator
	reputationTracker *IPReputationTracker
	arcPrivateKey     string
	srs               *srs.SRS
	domainLimiters    sync.Map // map[string]*domainRateLimiter
	metrics           DeliveryMetrics
	pool              *ConnectionPool
	stopCh            chan struct{}
}

type domainRateLimiter struct {
	mu         sync.Mutex
	tokens     float64
	lastUpdate time.Time
}

// DeliveryMetrics defines the interface for recording delivery metrics.
type DeliveryMetrics interface {
	RecordDeliveryAttempt(outcome, recipientDomain string, duration float64)
}

// NewDeliverer creates a new delivery engine.
func NewDeliverer(config *config.OutboundConfig, expandedIPs *config.ExpandedSourceIPs, mxLookup *MXLookup, logger *slog.Logger, reputationConfig *config.ReputationConfig, arcConfig *config.ARCConfig, srsConfig *config.SRSConfig) *Deliverer {
	// Create reputation tracker
	reputationTracker := NewIPReputationTracker(reputationConfig, logger)

	// Load ARC private key if enabled
	var arcPrivateKey string
	if arcConfig != nil && arcConfig.Enabled && arcConfig.PrivateKeyPath != "" {
		keyData, err := os.ReadFile(arcConfig.PrivateKeyPath)
		if err != nil {
			logger.Error("failed to read ARC private key", "error", err)
		} else {
			arcPrivateKey = string(keyData)
		}
	}

	// Initialize SRS if enabled
	var srsInstance *srs.SRS
	if srsConfig != nil && srsConfig.Enabled {
		if len(srsConfig.Domains) == 0 {
			logger.Error("SRS enabled but no domains configured")
		} else {
			var err error
			srsInstance, err = srs.NewSRS(
				srsConfig.Domains,
				srsConfig.Selection,
				srsConfig.Secret,
				srsConfig.MaxAge,
				srsConfig.HashLength,
				srsConfig.Separator,
				srsConfig.SkipDomains,
				srsConfig.SkipIfDKIMPass,
				srsConfig.SkipIfSameDomain,
				srsConfig.UseDynamicSubdomain,
			)
			if err != nil {
				logger.Error("failed to initialize SRS", "error", err)
				srsInstance = nil
			} else {
				logger.Info("SRS initialized", "domains", srsConfig.Domains, "selection", srsConfig.Selection)
			}
		}
	}

	ipsV4 := expandedIPs.IPv4
	ipsV6 := expandedIPs.IPv6

	d := &Deliverer{
		config:            config,
		arcConfig:         arcConfig,
		mxLookup:          mxLookup,
		logger:            logger,
		ipRotator:         NewIPRotator(ipsV4, ipsV6, config.SourceIPSelection),
		reputationTracker: reputationTracker,
		arcPrivateKey:     arcPrivateKey,
		srs:               srsInstance,
		pool:              NewConnectionPool(config.ConnectionPoolTTLSeconds, logger),
		stopCh:            make(chan struct{}),
	}

	// Start background cleanup goroutines
	d.startCleanupRoutines()

	return d
}

// startCleanupRoutines starts background goroutines for periodic cleanup of
// domain rate limiters, connection pool, MX cache, and IP reputation tracker.
func (d *Deliverer) startCleanupRoutines() {
	// Domain rate limiter cleanup (every 5 minutes, remove entries older than 10 minutes)
	recovery.SafeGo(d.logger, "domain-limiter-cleanup", func() {
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				d.cleanupDomainLimiters()
			case <-d.stopCh:
				return
			}
		}
	})

	// MX cache cleanup (every 5 minutes)
	recovery.SafeGo(d.logger, "mx-cache-cleanup", func() {
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				d.mxLookup.CleanupExpiredCache()
			case <-d.stopCh:
				return
			}
		}
	})

	// IP reputation cleanup (every hour)
	recovery.SafeGo(d.logger, "ip-reputation-cleanup", func() {
		ticker := time.NewTicker(1 * time.Hour)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				d.reputationTracker.Cleanup()
			case <-d.stopCh:
				return
			}
		}
	})
}

// cleanupDomainLimiters removes stale domain rate limiters that haven't been
// used for more than 10 minutes to prevent unbounded memory growth.
func (d *Deliverer) cleanupDomainLimiters() {
	cutoff := time.Now().Add(-10 * time.Minute)
	removed := 0

	d.domainLimiters.Range(func(key, value any) bool {
		limiter, ok := value.(*domainRateLimiter)
		if !ok {
			// Invalid type, remove it
			d.domainLimiters.Delete(key)
			removed++
			return true
		}

		limiter.mu.Lock()
		lastUpdate := limiter.lastUpdate
		limiter.mu.Unlock()

		if lastUpdate.Before(cutoff) {
			d.domainLimiters.Delete(key)
			removed++
		}
		return true
	})

	if removed > 0 {
		d.logger.Info("cleaned up stale domain rate limiters", "removed", removed)
	}
}

// Stop gracefully stops the deliverer and all background goroutines.
// It also closes the connection pool and cleans up resources.
func (d *Deliverer) Stop() {
	d.logger.Info("stopping deliverer...")

	// Signal all background goroutines to stop
	close(d.stopCh)

	// Close connection pool
	if d.pool != nil {
		d.pool.CloseAll()
	}

	// Final cleanup
	d.reputationTracker.Cleanup()

	d.logger.Info("deliverer stopped")
}

// getConfig returns the current outbound config under read lock.
func (d *Deliverer) getConfig() *config.OutboundConfig {
	d.configMu.RLock()
	defer d.configMu.RUnlock()
	return d.config
}

// ReloadConfig hot-swaps the outbound configuration.
func (d *Deliverer) ReloadConfig(cfg *config.OutboundConfig) {
	d.configMu.Lock()
	defer d.configMu.Unlock()
	d.config = cfg
	d.logger.Info("deliverer config reloaded")
}

// GetConnectionPool returns the connection pool (for testing/inspection)
func (d *Deliverer) GetConnectionPool() *ConnectionPool {
	return d.pool
}

// GetReputationTracker returns the IP reputation tracker (for testing/inspection)
func (d *Deliverer) GetReputationTracker() *IPReputationTracker {
	return d.reputationTracker
}

// fixedDestination returns the configured fixed "host:port" destination for the
// given protocol, or "" if MX lookup should be used.
func (d *Deliverer) fixedDestination(cfg *config.OutboundConfig, protocol string) string {
	if protocol == config.ProtocolLMTP {
		return cfg.DefaultLMTPDestination
	}
	return cfg.DefaultSMTPDestination
}

// DeliverMessage attempts to deliver a message synchronously.
// transport overrides the config default_protocol when non-empty ("smtp" or "lmtp").
func (d *Deliverer) DeliverMessage(ctx context.Context, from, to string, message []byte, transport string, dkimPrivateKey, dkimSelector, dkimDomain string, skipDKIMValidation bool, arcPrivateKey, arcSelector, arcDomain string, inboundAuth *InboundAuthResults) DeliveryResult {
	start := time.Now()
	cfg := d.getConfig()

	// Resolve effective protocol: explicit transport > config default
	protocol := transport
	if protocol == "" {
		protocol = cfg.DefaultProtocol
	}

	// Extract trace ID from context (generated by the handler per request).
	traceID := TraceIDFromContext(ctx)
	logger := d.logger.With("trace_id", traceID)

	// Extract domain from recipient
	_, domain := splitEmail(to)
	if domain == "" {
		logger.Debug("invalid recipient address", "to", to)
		result := DeliveryResult{
			TraceID: traceID,
			Status:  "hard_bounce",
			Error:   "Invalid recipient address",
		}
		d.logDeliveryResult(logger, from, to, result)
		return result
	}

	logger.Info("starting delivery attempt", "from", from, "to", to, "domain", domain)

	// 1. Wait for per-domain rate limit (skip if disabled or whitelisted)
	if cfg.PerDomainIntervalSeconds > 0 && !d.isDomainWhitelisted(cfg, domain) {
		if err := d.waitForDomainRateLimit(ctx, cfg, domain); err != nil {
			if err == ErrDomainRateLimitExceeded {
				logger.Warn("domain rate limit exceeded", "domain", domain)
				result := DeliveryResult{
					TraceID: traceID,
					Status:  "rate_limit", // Fail Fast status
					Error:   "Domain rate limit exceeded",
				}
				d.logDeliveryResult(logger, from, to, result)
				return result
			}
			logger.Debug("rate limit check failed", "domain", domain, "error", err)
			result := DeliveryResult{
				TraceID: traceID,
				Status:  "timeout",
				Error:   "Rate limit check failed",
			}
			d.logDeliveryResult(logger, from, to, result)
			return result
		}
	}

	// 2. DKIM Signing (if provided)
	signedMessage := message
	if dkimPrivateKey != "" && dkimSelector != "" {
		// Use provided domain or extract from sender
		signingDomain := dkimDomain
		if signingDomain == "" {
			signingDomain = dkim.ExtractDomainFromEmail(from)
			if signingDomain == "" {
				logger.Debug("failed to extract domain from sender for DKIM", "from", from)
				result := DeliveryResult{
					TraceID: traceID,
					Status:  "error",
					Error:   "Invalid sender address for DKIM signing",
				}
				d.logDeliveryResult(logger, from, to, result)
				return result
			}
		}

		// Validate DKIM configuration (check DNS record and key match) unless skipped
		dkimValid := true
		if !skipDKIMValidation {
			logger.Debug("validating DKIM configuration", "selector", dkimSelector, "domain", signingDomain)
			if err := dkim.ValidateDKIMConfiguration(ctx, dkimSelector, signingDomain, dkimPrivateKey); err != nil {
				logger.Warn("DKIM validation failed, will deliver without DKIM signature", "error", err, "selector", dkimSelector, "domain", signingDomain)
				dkimValid = false
			}
		} else {
			logger.Debug("skipping DKIM validation (skip_dkim_validation=true)", "selector", dkimSelector, "domain", signingDomain)
		}

		if dkimValid {
			logger.Debug("signing message with DKIM", "selector", dkimSelector, "domain", signingDomain)
			signed, err := dkim.SignMessage(message, dkimPrivateKey, dkimSelector, signingDomain)
			if err != nil {
				logger.Warn("DKIM signing failed, will deliver without DKIM signature", "error", err, "selector", dkimSelector, "domain", signingDomain)
			} else {
				signedMessage = signed
				logger.Debug("message signed with DKIM", "original_size", len(message), "signed_size", len(signedMessage))
			}
		} else {
			logger.Debug("skipping DKIM signing due to validation failure")
		}
	}

	// 3. ARC Signing (if provided via API or enabled in config)
	// Priority: API parameters > config
	finalARCPrivateKey := arcPrivateKey
	finalARCSelector := arcSelector
	finalARCDomain := arcDomain

	logger.Debug("ARC signing check",
		"has_api_key", arcPrivateKey != "",
		"api_selector", arcSelector,
		"api_domain", arcDomain)

	// Use config defaults if not provided via API
	if finalARCPrivateKey == "" && d.arcConfig != nil && d.arcConfig.Enabled && d.arcPrivateKey != "" {
		finalARCPrivateKey = d.arcPrivateKey
		logger.Debug("using ARC private key from config")
	}
	if finalARCSelector == "" && d.arcConfig != nil && d.arcConfig.Selector != "" {
		finalARCSelector = d.arcConfig.Selector
		logger.Debug("using ARC selector from config", "selector", finalARCSelector)
	}
	if finalARCDomain == "" && d.arcConfig != nil && d.arcConfig.Domain != "" {
		finalARCDomain = d.arcConfig.Domain
		logger.Debug("using ARC domain from config", "domain", finalARCDomain)
	}

	logger.Debug("final ARC parameters",
		"has_private_key", finalARCPrivateKey != "",
		"selector", finalARCSelector,
		"domain", finalARCDomain,
		"will_sign", finalARCPrivateKey != "" && finalARCSelector != "" && finalARCDomain != "")

	// Apply ARC signing if we have all required parameters
	if finalARCPrivateKey != "" && finalARCSelector != "" && finalARCDomain != "" {
		logger.Info("applying ARC signing", "selector", finalARCSelector, "domain", finalARCDomain)
		arcConfig := &arc.SignConfig{
			Selector:    finalARCSelector,
			Domain:      finalARCDomain,
			PrivateKey:  finalARCPrivateKey,
			HeaderCanon: "relaxed",
			BodyCanon:   "relaxed",
		}
		arcSigned, err := arc.SignMessage(signedMessage, arcConfig)
		if err != nil {
			logger.Warn("ARC signing failed, will deliver without ARC signature", "error", err)
		} else {
			signedMessage = arcSigned
			logger.Info("message signed with ARC successfully", "original_size", len(message), "arc_signed_size", len(signedMessage))
		}
	} else {
		// Warn if ARC is partially configured (some params set but not all)
		hasKey := finalARCPrivateKey != ""
		hasSel := finalARCSelector != ""
		hasDom := finalARCDomain != ""
		if hasKey || hasSel || hasDom {
			logger.Warn("ARC signing skipped: incomplete configuration (need all of: private_key, selector, domain)",
				"has_private_key", hasKey,
				"has_selector", hasSel,
				"has_domain", hasDom)
		} else {
			logger.Debug("ARC signing not configured")
		}
	}

	// 4. Lookup MX records (skip if LMTP mode or fixed SMTP destination)
	var mxRecords []*MXRecord
	if fixedDest := d.fixedDestination(cfg, protocol); fixedDest != "" {
		logger.Info("using fixed destination", "protocol", protocol, "destination", fixedDest)
		host, _, err := net.SplitHostPort(fixedDest)
		if err != nil {
			result := DeliveryResult{
				TraceID: traceID,
				Status:  "error",
				Error:   fmt.Sprintf("Invalid %s destination %q: %v", protocol, fixedDest, err),
			}
			d.logDeliveryResult(logger, from, to, result)
			return result
		}
		mxRecords = []*MXRecord{{Host: host, Priority: 0}}
	} else {
		// SMTP mode: lookup MX records
		logger.Debug("looking up MX records", "domain", domain)
		var err error
		mxRecords, err = d.mxLookup.Lookup(ctx, domain)
		if err != nil {
			logger.Debug("MX lookup failed", "domain", domain, "error", err)
			result := DeliveryResult{
				TraceID: traceID,
				Status:  "temp_fail",
				Error:   fmt.Sprintf("MX lookup failed: %v", err),
			}
			d.logDeliveryResult(logger, from, to, result)
			return result
		}
		if len(mxRecords) == 0 {
			logger.Debug("no MX records found", "domain", domain)
			result := DeliveryResult{
				TraceID: traceID,
				Status:  "hard_bounce",
				Error:   "No MX records found",
			}
			d.logDeliveryResult(logger, from, to, result)
			return result
		}

		logger.Info("found MX records", "domain", domain, "count", len(mxRecords))
	}

	// 5. Try each MX with IPv6/IPv4 preference
	var lastResult DeliveryResult

	// Determine delivery order based on per-protocol IP mode.
	// In dual mode, the prefer_ipv6 setting controls which version is tried first.
	// tryIPv4/tryIPv6 control source-IP-bound delivery paths.
	// preferIPv6 controls target IP selection when no source IPs are configured.
	ipMode := cfg.IPModeForProtocol(protocol)
	preferIPv6 := cfg.PreferIPv6ForProtocol(protocol)
	tryIPv4 := (ipMode == config.IPModeDual || ipMode == config.IPModeIPv4) && d.ipRotator.HasIPv4()
	tryIPv6 := (ipMode == config.IPModeDual || ipMode == config.IPModeIPv6) && d.ipRotator.HasIPv6()
	// In dual mode, tryIPv6First is determined by preferIPv6 setting
	// In IPv6-only mode, always try IPv6 first (tryIPv4 will be false anyway)
	tryIPv6First := tryIPv6 && (ipMode == config.IPModeIPv6 || (ipMode == config.IPModeDual && preferIPv6))

	for i, mx := range mxRecords {
		logger.Info("trying MX", "host", mx.Host, "priority", mx.Priority, "index", i, "total", len(mxRecords))

		// Check context
		if ctx.Err() != nil {
			logger.Debug("context canceled during MX attempts", "error", ctx.Err())
			lastResult = DeliveryResult{TraceID: traceID, Status: "timeout", Error: "Context canceled", MXHost: mx.Host}
			break
		}

		// Pre-check: Does this MX host have IPv4/IPv6 addresses?
		mxHasIPv4, mxHasIPv6 := d.checkMXIPVersions(ctx, logger, mx.Host)

		// Try IPv6 first if preferred
		if tryIPv6First {
			// Only try IPv6 if MX actually has IPv6 addresses
			if tryIPv6 && mxHasIPv6 {
				logger.Debug("trying IPv6 first", "mx", mx.Host)
				result := d.tryDeliveryWithIPVersion(ctx, logger, traceID, from, to, signedMessage, mx.Host, true, start, protocol, inboundAuth)
				// Return immediately for definitive results (don't try other MX servers)
				if result.Status == "delivered" || result.Status == "hard_bounce" || result.Status == "temp_fail" {
					result.AttemptDurationMs = time.Since(start).Milliseconds()
					d.recordMetrics(result, domain)
					d.logDeliveryResult(logger, from, to, result)
					return result
				}
				lastResult = result
			} else if tryIPv6 && !mxHasIPv6 {
				logger.Debug("skipping IPv6 attempt, MX has no IPv6 addresses", "mx", mx.Host)
			}

			// Fall back to IPv4 if IPv6 failed (or was skipped) and IPv4 is available
			if tryIPv4 && mxHasIPv4 {
				logger.Debug("falling back to IPv4", "mx", mx.Host)
				result := d.tryDeliveryWithIPVersion(ctx, logger, traceID, from, to, signedMessage, mx.Host, false, start, protocol, inboundAuth)
				if result.Status == "delivered" || result.Status == "hard_bounce" || result.Status == "temp_fail" {
					result.AttemptDurationMs = time.Since(start).Milliseconds()
					d.recordMetrics(result, domain)
					d.logDeliveryResult(logger, from, to, result)
					return result
				}
				lastResult = result
			} else if tryIPv4 && !mxHasIPv4 {
				logger.Debug("skipping IPv4 attempt, MX has no IPv4 addresses", "mx", mx.Host)
			}
		} else if tryIPv4 {
			// Try IPv4 first (or only)
			if mxHasIPv4 {
				logger.Debug("trying IPv4", "mx", mx.Host)
				result := d.tryDeliveryWithIPVersion(ctx, logger, traceID, from, to, signedMessage, mx.Host, false, start, protocol, inboundAuth)
				if result.Status == "delivered" || result.Status == "hard_bounce" || result.Status == "temp_fail" {
					result.AttemptDurationMs = time.Since(start).Milliseconds()
					d.recordMetrics(result, domain)
					d.logDeliveryResult(logger, from, to, result)
					return result
				}
				lastResult = result
			} else {
				logger.Debug("skipping IPv4 attempt, MX has no IPv4 addresses", "mx", mx.Host)
			}

			// Fall back to IPv6 if IPv4 failed (or was skipped) and IPv6 is available
			if tryIPv6 && mxHasIPv6 {
				logger.Debug("falling back to IPv6", "mx", mx.Host)
				result := d.tryDeliveryWithIPVersion(ctx, logger, traceID, from, to, signedMessage, mx.Host, true, start, protocol, inboundAuth)
				if result.Status == "delivered" || result.Status == "hard_bounce" || result.Status == "temp_fail" {
					result.AttemptDurationMs = time.Since(start).Milliseconds()
					d.recordMetrics(result, domain)
					d.logDeliveryResult(logger, from, to, result)
					return result
				}
				lastResult = result
			} else if tryIPv6 && !mxHasIPv6 {
				logger.Debug("skipping IPv6 attempt, MX has no IPv6 addresses", "mx", mx.Host)
			}
		} else {
			// No source IPs configured - use system default
			logger.Debug("no source IPs configured, using system default", "mx", mx.Host)
			result := d.attemptDelivery(ctx, logger, traceID, from, to, signedMessage, mx.Host, "", preferIPv6, protocol, inboundAuth)
			deliveryInfo := DeliveryInfo{From: from, To: to, MXHost: mx.Host}
			d.reputationTracker.RecordDeliveryAttempt("", result.Status == "delivered", nil, deliveryInfo)
			if result.Status == "delivered" || result.Status == "hard_bounce" || result.Status == "temp_fail" {
				result.AttemptDurationMs = time.Since(start).Milliseconds()
				d.recordMetrics(result, domain)
				d.logDeliveryResult(logger, from, to, result)
				return result
			}
			lastResult = result
		}

		// For timeout/error results, continue to next MX server
		// (delivered/hard_bounce/temp_fail already returned above)
	}

	lastResult.AttemptDurationMs = time.Since(start).Milliseconds()
	d.recordMetrics(lastResult, domain)
	d.logDeliveryResult(logger, from, to, lastResult)
	return lastResult
}

// checkMXIPVersions checks if an MX host has IPv4 and/or IPv6 addresses.
// Returns (hasIPv4, hasIPv6).
func (d *Deliverer) checkMXIPVersions(ctx context.Context, logger *slog.Logger, mxHost string) (bool, bool) {
	mxIPs, err := d.mxLookup.dnsResolver.LookupHost(ctx, mxHost)
	if err != nil {
		logger.Debug("failed to check MX IP versions", "mx", mxHost, "error", err)
		// On DNS error, assume both are available (let actual delivery fail properly)
		return true, true
	}

	hasIPv4, hasIPv6 := false, false
	for _, ip := range mxIPs {
		parsedIP := net.ParseIP(ip)
		if parsedIP == nil {
			continue
		}
		if parsedIP.To4() != nil {
			hasIPv4 = true
		} else {
			hasIPv6 = true
		}
		// Early exit if we've found both
		if hasIPv4 && hasIPv6 {
			break
		}
	}

	logger.Debug("MX IP version check", "mx", mxHost, "has_ipv4", hasIPv4, "has_ipv6", hasIPv6)
	return hasIPv4, hasIPv6
}

// tryDeliveryWithIPVersion attempts delivery using either IPv6 or IPv4 source IPs and matching MX host IPs.
func (d *Deliverer) tryDeliveryWithIPVersion(ctx context.Context, logger *slog.Logger, traceID string, from, to string, message []byte, mxHost string, useIPv6 bool, startTime time.Time, protocol string, inboundAuth *InboundAuthResults) DeliveryResult {
	// Extract domain from recipient for hash-domain strategy
	_, domain := splitEmail(to)

	logger.Debug("selecting source IPs", "ipv6", useIPv6, "domain", domain)

	// Get ordered source IPs for this version based on rotation strategy
	sourceIPs := d.ipRotator.SelectIPs(useIPv6, domain)

	// Filter by reputation
	healthyIPs := d.reputationTracker.GetHealthyIPs(sourceIPs)
	if len(healthyIPs) == 0 && len(sourceIPs) > 0 {
		logger.Warn("all source IPs degraded for IP version",
			"ipv6", useIPv6,
			"domain", to)
		// Use degraded IPs as last resort
		healthyIPs = sourceIPs
	}

	// Try each healthy source IP
	var lastResult DeliveryResult
	for i, sourceIP := range healthyIPs {
		logger.Info("attempting delivery with source IP",
			"mx", mxHost,
			"source_ip", sourceIP,
			"ipv6", useIPv6,
			"index", i,
			"total", len(healthyIPs))

		// Check context
		if ctx.Err() != nil {
			logger.Debug("context canceled during source IP attempts", "error", ctx.Err(), "mx", mxHost, "source_ip", sourceIP)
			return DeliveryResult{TraceID: traceID, Status: "timeout", Error: "Context canceled", MXHost: mxHost, SourceIP: sourceIP}
		}

		result := d.attemptDelivery(ctx, logger, traceID, from, to, message, mxHost, sourceIP, useIPv6, protocol, inboundAuth)
		lastResult = result

		// Track reputation
		deliveryInfo := DeliveryInfo{From: from, To: to, MXHost: mxHost}
		d.reputationTracker.RecordDeliveryAttempt(sourceIP, result.Status == "delivered", nil, deliveryInfo)

		// Return immediately for definitive results or server-side temp failures
		if result.Status == "delivered" || result.Status == "hard_bounce" || result.Status == "timeout" || result.Status == "temp_fail" {
			return result
		}

		// For network errors or connection failures, try next source IP
		logger.Debug("delivery attempt failed, trying next IP",
			"mx", mxHost,
			"source_ip", sourceIP,
			"ipv6", useIPv6,
			"status", result.Status,
			"error", result.Error)
	}

	return lastResult
}

// isDomainWhitelisted checks if a domain is in the rate limit whitelist.
func (d *Deliverer) isDomainWhitelisted(cfg *config.OutboundConfig, domain string) bool {
	if len(cfg.RateLimitWhitelist) == 0 {
		return false
	}

	// Case-insensitive domain matching
	domainLower := strings.ToLower(domain)
	for _, whitelistedDomain := range cfg.RateLimitWhitelist {
		if strings.ToLower(whitelistedDomain) == domainLower {
			d.logger.Debug("domain is whitelisted from rate limiting", "domain", domain)
			return true
		}
	}
	return false
}

func (d *Deliverer) waitForDomainRateLimit(ctx context.Context, cfg *config.OutboundConfig, domain string) error {
	initialTokens := float64(cfg.PerDomainBurst)
	if initialTokens <= 0 {
		initialTokens = 1
	}

	limiterI, _ := d.domainLimiters.LoadOrStore(domain, &domainRateLimiter{
		tokens:     initialTokens,
		lastUpdate: time.Now(),
	})
	limiter, ok := limiterI.(*domainRateLimiter)
	if !ok {
		d.logger.Error("type assertion failed for domain rate limiter",
			"domain", domain,
			"type", fmt.Sprintf("%T", limiterI))
		return fmt.Errorf("internal error: invalid rate limiter type")
	}

	limiter.mu.Lock()
	defer limiter.mu.Unlock()

	now := time.Now()
	interval := time.Duration(cfg.PerDomainIntervalSeconds) * time.Second
	burst := float64(cfg.PerDomainBurst)
	if burst <= 0 {
		burst = 1
	}

	// Calculate refill rate (tokens per second)
	var rate float64
	if interval > 0 {
		rate = 1.0 / interval.Seconds()
	} else {
		rate = 1000000.0 // Effective infinity
	}

	// Refill tokens based on elapsed time
	elapsed := now.Sub(limiter.lastUpdate).Seconds()
	// Initialize lastUpdate if this is a fresh limiter (though LoadOrStore init handles it, race conditions might create one with zero time)
	if limiter.lastUpdate.IsZero() {
		limiter.tokens = burst
	} else {
		limiter.tokens += elapsed * rate
		if limiter.tokens > burst {
			limiter.tokens = burst
		}
	}
	limiter.lastUpdate = now

	// Consume token
	if limiter.tokens >= 1.0 {
		limiter.tokens -= 1.0
		return nil
	}

	// Fail Fast - No tokens available
	return ErrDomainRateLimitExceeded
}

func (d *Deliverer) attemptDelivery(ctx context.Context, logger *slog.Logger, traceID string, from, to string, msg []byte, mxHost, sourceIP string, preferIPv6 bool, protocol string, inboundAuth *InboundAuthResults) DeliveryResult {
	// LMTP: no connection pooling, raw protocol
	if protocol == config.ProtocolLMTP {
		dr, result, err := d.dialAndHello(ctx, logger, traceID, mxHost, sourceIP, preferIPv6, protocol)
		if err != nil {
			return result
		}
		return d.performLMTPTransaction(ctx, logger, traceID, dr.Conn, dr.Reader, from, to, msg, mxHost, sourceIP)
	}

	// SMTP: try pooled connection first
	var client *smtp.Client
	if d.pool != nil {
		client = d.pool.Get(mxHost, sourceIP)
		if client != nil {
			logger.Debug("using pooled connection", "mx", mxHost)

			result := d.performDeliveryTransaction(ctx, logger, traceID, client, from, to, msg, mxHost, sourceIP, true, inboundAuth)
			if result.Status == "delivered" || result.Status == "hard_bounce" || result.Status == "timeout" {
				return result
			}

			logger.Debug("pooled connection failed, attempting with fresh connection",
				"mx", mxHost,
				"error", result.Error)
			client = nil
		}
	}

	if client == nil {
		dr, result, err := d.dialAndHello(ctx, logger, traceID, mxHost, sourceIP, preferIPv6, protocol)
		if err != nil {
			return result
		}
		client = dr.Client
	}

	return d.performDeliveryTransaction(ctx, logger, traceID, client, from, to, msg, mxHost, sourceIP, false, inboundAuth)
}

func (d *Deliverer) performDeliveryTransaction(ctx context.Context, logger *slog.Logger, traceID string, client *smtp.Client, from, to string, msg []byte, mxHost, sourceIP string, reused bool, inboundAuth *InboundAuthResults) DeliveryResult {
	// Check context before starting transaction
	if ctx.Err() != nil {
		client.Close()
		return DeliveryResult{TraceID: traceID, Status: "timeout", MXHost: mxHost, SourceIP: sourceIP, Error: "context cancelled before delivery transaction"}
	}

	// Tighten client timeouts to remaining context deadline so SMTP commands
	// do not outlive the caller's deadline.
	if deadline, ok := ctx.Deadline(); ok {
		remaining := time.Until(deadline)
		const minSMTPTimeout = 5 * time.Second
		if remaining > minSMTPTimeout {
			if remaining < client.CommandTimeout {
				client.CommandTimeout = remaining
			}
			if remaining < client.SubmissionTimeout {
				client.SubmissionTimeout = remaining
			}
		}
		// If remaining <= minSMTPTimeout, don't tighten further — let the
		// context deadline itself enforce cancellation rather than setting
		// unreasonably small SMTP timeouts.
	}

	logger.Debug("starting delivery transaction",
		"mx", mxHost,
		"source_ip", sourceIP,
		"reused", reused)

	smtpMsg, err := d.deliverPayload(ctx, logger, client, from, to, msg, inboundAuth)
	if err != nil {
		// If error occurred on a reused connection, it might be stale.
		// We could retry? For now, we fail and let client retry.
		client.Close()

		// If reused and error is network/EOF, we might want to suggest retry.
		return d.mapSMTPError(logger, traceID, err, mxHost, sourceIP)
	}

	// Success!
	// Reset and put back in pool
	if err := client.Reset(); err == nil && d.pool != nil {
		d.pool.Put(client, mxHost, sourceIP)
	} else {
		// If Reset failed, connection is dirty/dead.
		client.Quit()
	}

	return DeliveryResult{
		TraceID:     traceID,
		Status:      "delivered",
		SMTPCode:    250,
		SMTPMessage: smtpMsg,
		MXHost:      mxHost,
		SourceIP:    sourceIP,
	}
}

// shouldSkipSRS delegates to d.srs.ShouldSkip, keeping delivery.go free of skip policy details.
func (d *Deliverer) shouldSkipSRS(from, to string, inboundAuth *InboundAuthResults) (skip bool, reason string) {
	dkimResult := ""
	if inboundAuth != nil {
		dkimResult = inboundAuth.DKIM
	}
	return d.srs.ShouldSkip(from, to, dkimResult)
}

func (d *Deliverer) deliverPayload(ctx context.Context, logger *slog.Logger, client *smtp.Client, from, to string, msg []byte, inboundAuth *InboundAuthResults) (string, error) {
	// Accept callers that pass envelope addresses with or without angle brackets.
	// Null sender ("" or "<>") both result in MAIL FROM:<>, since go-smtp wraps
	// the address in <> itself.
	from = stripAngleBrackets(from)
	to = stripAngleBrackets(to)

	// MAIL FROM — SRS rewrite for SMTP (LMTP uses performLMTPTransaction instead)
	mailFrom := from
	if d.srs != nil {
		if skip, reason := d.shouldSkipSRS(from, to, inboundAuth); skip {
			logger.Debug("SRS skipped", "reason", reason, "from", from, "to", to)
		} else if rewritten, err := d.srs.Forward(from); err == nil {
			mailFrom = rewritten
			logger.Info("SRS rewrote sender", "original", from, "rewritten", rewritten)
		} else {
			logger.Warn("SRS rewrite failed", "original", from, "error", err)
		}
	}

	// Check if UTF-8 addresses are needed and supported
	needsUTF8 := needsUTF8Address(mailFrom) || needsUTF8Address(to)
	var mailOpts *smtp.MailOptions
	if needsUTF8 {
		// Only use SMTPUTF8 if server supports it
		if supportsExtension(client, "SMTPUTF8") {
			mailOpts = &smtp.MailOptions{UTF8: true}
			logger.Debug("using SMTPUTF8 extension", "from", mailFrom, "to", to)
		} else {
			// Server doesn't support UTF-8, delivery will likely fail
			logger.Warn("UTF-8 address required but server doesn't support SMTPUTF8", "from", mailFrom, "to", to)
		}
	}

	// Check context before MAIL FROM
	if err := ctx.Err(); err != nil {
		return "", fmt.Errorf("context cancelled before MAIL FROM: %w", err)
	}

	// Log envelope and first message headers at DEBUG for diagnostics
	if logger.Enabled(ctx, slog.LevelDebug) {
		logger.Debug("SMTP envelope",
			"mail_from", mailFrom,
			"original_from", from,
			"rcpt_to", to,
			"srs_applied", mailFrom != from,
			"message_headers", extractHeaders(msg))
	}

	logger.Debug("sending MAIL FROM", "from", mailFrom)
	if err := client.Mail(mailFrom, mailOpts); err != nil {
		logger.Debug("MAIL FROM failed", "error", err)
		return "", err
	}

	// Check context before RCPT TO
	if err := ctx.Err(); err != nil {
		return "", fmt.Errorf("context cancelled before RCPT TO: %w", err)
	}

	// RCPT TO
	logger.Debug("sending RCPT TO", "to", to)
	if err := client.Rcpt(to, nil); err != nil {
		logger.Debug("RCPT TO failed", "error", err)
		return "", err
	}

	// Check context before DATA
	if err := ctx.Err(); err != nil {
		return "", fmt.Errorf("context cancelled before DATA: %w", err)
	}

	// Prepend X-Envelope-To so downstream relays can identify the envelope recipient
	msg = append([]byte("X-Envelope-To: "+to+"\r\n"), msg...)

	// DATA
	logger.Debug("sending DATA", "size", len(msg))
	w, err := client.Data()
	if err != nil {
		logger.Debug("DATA command failed", "error", err)
		return "", err
	}
	if w == nil {
		return "", fmt.Errorf("DATA command returned nil writer")
	}

	if _, err := w.Write(msg); err != nil {
		logger.Debug("failed to write message data", "error", err)
		return "", err
	}

	// Close and capture the SMTP response from the server
	resp, err := w.CloseWithResponse()
	if err != nil {
		logger.Debug("DATA close failed (message rejected)", "error", err)
		return "", err
	}

	// Normalize multi-line SMTP responses by replacing newlines with spaces
	// (textproto.ReadResponse returns multi-line messages with \n separators)
	smtpMsg := strings.ReplaceAll(resp.StatusText, "\n", " ")

	logger.Debug("delivery transaction successful", "smtp_response", smtpMsg)
	return smtpMsg, nil
}

// dialResult holds the outcome of dialAndHello. For SMTP, Client is set.
// For LMTP, Conn and Reader are set (raw protocol, go-smtp doesn't support LMTP DATA responses).
type dialResult struct {
	Client *smtp.Client
	Conn   net.Conn      // raw connection for LMTP
	Reader *bufio.Reader // buffered reader for LMTP (preserves state from handshake)
}

func (d *Deliverer) dialAndHello(ctx context.Context, logger *slog.Logger, traceID string, mxHost, sourceIP string, preferIPv6 bool, protocol string) (*dialResult, DeliveryResult, error) {
	cfg := d.getConfig()
	logger.Debug("resolving MX host", "mx", mxHost)

	// Resolve MX host to IP addresses
	mxIPs, err := d.mxLookup.dnsResolver.LookupHost(ctx, mxHost)
	if err != nil {
		return nil, DeliveryResult{
			TraceID:  traceID,
			Status:   "temp_fail",
			MXHost:   mxHost,
			SourceIP: sourceIP,
			Error:    fmt.Sprintf("Failed to resolve MX host: %v", err),
		}, err
	}

	// Filter MX IPs by version matching sourceIP if provided, or preferIPv6
	var targetIP string
	isSourceIPv6 := false
	if sourceIP != "" {
		parsedSource := net.ParseIP(sourceIP)
		isSourceIPv6 = parsedSource != nil && parsedSource.To4() == nil
	} else {
		isSourceIPv6 = preferIPv6
	}

	logger.Debug("selecting target MX IP", "mx", mxHost, "source_ip", sourceIP, "require_ipv6", isSourceIPv6)

	for _, ip := range mxIPs {
		parsedIP := net.ParseIP(ip)
		if parsedIP == nil {
			continue
		}

		isTargetV4 := parsedIP.To4() != nil
		if isSourceIPv6 && !isTargetV4 {
			targetIP = ip
			break
		} else if !isSourceIPv6 && isTargetV4 {
			targetIP = ip
			break
		}
	}

	// If no matching IP version found, this should not normally happen since we pre-check
	// MX IP versions before calling tryDeliveryWithIPVersion. If we reach here, something
	// changed between the check and the delivery attempt (e.g., DNS TTL expired).
	if targetIP == "" {
		if len(mxIPs) > 0 {
			ipVersion := "IPv4"
			if isSourceIPv6 {
				ipVersion = "IPv6"
			}
			logger.Warn("no matching IP version for MX (unexpected - should have been pre-filtered)",
				"mx", mxHost,
				"required_version", ipVersion,
				"available_ips", mxIPs)
			return nil, DeliveryResult{
				TraceID:  traceID,
				Status:   "temp_fail",
				MXHost:   mxHost,
				SourceIP: sourceIP,
				Error:    fmt.Sprintf("MX host has no %s addresses", ipVersion),
			}, fmt.Errorf("no matching IP version")
		} else {
			return nil, DeliveryResult{
				TraceID:  traceID,
				Status:   "temp_fail",
				MXHost:   mxHost,
				SourceIP: sourceIP,
				Error:    "No IP addresses resolved for MX host",
			}, fmt.Errorf("no IP")
		}
	}

	// Determine target port based on protocol and configured destinations
	var targetPort string
	if protocol == config.ProtocolLMTP {
		targetPort = fmt.Sprintf("%d", cfg.LMTPPort)
	} else {
		targetPort = fmt.Sprintf("%d", cfg.SMTPPort)
	}
	if fixedDest := d.fixedDestination(cfg, protocol); fixedDest != "" {
		_, port, err := net.SplitHostPort(fixedDest)
		if err != nil {
			return nil, DeliveryResult{
				TraceID:  traceID,
				Status:   "error",
				MXHost:   mxHost,
				SourceIP: sourceIP,
				Error:    fmt.Sprintf("Invalid %s destination %q: %v", protocol, fixedDest, err),
			}, err
		}
		targetPort = port
	}

	// TCP connection to target port
	// Use the minimum of configured timeout and remaining context deadline
	connectionTimeout := time.Duration(cfg.ConnectionTimeoutSeconds) * time.Second
	if deadline, ok := ctx.Deadline(); ok {
		remaining := time.Until(deadline)
		if remaining < connectionTimeout {
			connectionTimeout = remaining
		}
	}

	dialer := &net.Dialer{
		Timeout: connectionTimeout,
	}

	if sourceIP != "" {
		ip := net.ParseIP(sourceIP)
		if ip == nil {
			return nil, DeliveryResult{
				TraceID:  traceID,
				Status:   "error",
				MXHost:   mxHost,
				SourceIP: sourceIP,
				Error:    fmt.Sprintf("Invalid source IP address: %s", sourceIP),
			}, fmt.Errorf("invalid IP")
		}

		// Bind to source IP
		// Note: IP version matching is handled earlier in target IP selection,
		// so we should never reach here with mismatched versions
		dialer.LocalAddr = &net.TCPAddr{IP: ip}
	}

	logger.Info("connecting to MX", "mx", mxHost, "target_ip", targetIP, "port", targetPort, "source_ip", sourceIP, "protocol", protocol)

	// Connect to the target IP
	conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(targetIP, targetPort))
	if err != nil {
		logger.Debug("TCP dial failed", "mx", mxHost, "target_ip", targetIP, "source_ip", sourceIP, "error", err)

		// Determine status based on error classification
		status := "temp_fail"
		classified := ClassifyError(0, "", err)
		if classified.Category == ErrorNetwork && (strings.Contains(strings.ToLower(err.Error()), "deadline") || strings.Contains(strings.ToLower(err.Error()), "timeout")) {
			// If it was a timeout, check if it was the context
			if ctx.Err() != nil {
				status = "timeout"
			}
		}

		// Check if error is due to source IP binding failure
		if sourceIP != "" && isBindError(err) {
			logger.Error("failed to bind to source IP",
				"source_ip", sourceIP,
				"target_ip", targetIP,
				"error", err)
			return nil, DeliveryResult{
				TraceID:  traceID,
				Status:   "error",
				MXHost:   mxHost,
				SourceIP: sourceIP,
				Error:    fmt.Sprintf("Cannot bind to source IP %s for target %s: %v", sourceIP, targetIP, err),
			}, err
		}
		return nil, DeliveryResult{TraceID: traceID, Status: status, MXHost: mxHost, SourceIP: sourceIP, Error: err.Error()}, err
	}

	// Track whether conn has been closed (by the goroutine or ctx.Done).
	// Uses atomic.Bool because it's written by the goroutine and read by
	// the outer function concurrently.
	var connClosed atomic.Bool
	// Ensure connection is closed if setup fails.
	success := false
	defer func() {
		if !success && !connClosed.Load() {
			conn.Close()
		}
	}()

	// Extract hostname for TLS verification
	host, _, _ := net.SplitHostPort(mxHost)
	if host == "" {
		host = mxHost
	}
	// Trim trailing dot for TLS verification (MX records often have them)
	host = strings.TrimSuffix(host, ".")

	// Calculate phased timeouts from context deadline or config defaults
	var bannerTimeout, handshakeTimeout, deliveryTimeout time.Duration

	if deadline, ok := ctx.Deadline(); ok {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil, DeliveryResult{TraceID: traceID, Status: "timeout", MXHost: mxHost, SourceIP: sourceIP, Error: "context deadline exceeded before handshake"}, ctx.Err()
		}

		// Calculate desired timeouts from config
		desiredBanner := time.Duration(cfg.BannerTimeoutSeconds) * time.Second
		desiredHandshake := time.Duration(cfg.HandshakeTimeoutSeconds) * time.Second
		desiredDelivery := time.Duration(cfg.SMTPTimeoutSeconds) * time.Second
		totalDesired := desiredBanner + desiredHandshake + desiredDelivery

		// If we have less time than desired, scale proportionally
		if remaining < totalDesired {
			// Scale down all timeouts proportionally
			scale := float64(remaining) / float64(totalDesired)
			bannerTimeout = time.Duration(float64(desiredBanner) * scale)
			handshakeTimeout = time.Duration(float64(desiredHandshake) * scale)
			deliveryTimeout = time.Duration(float64(desiredDelivery) * scale)
			logger.Debug("scaled timeouts due to context deadline",
				"remaining", remaining,
				"banner_timeout", bannerTimeout,
				"handshake_timeout", handshakeTimeout,
				"delivery_timeout", deliveryTimeout)
		} else {
			// Use full desired timeouts
			bannerTimeout = desiredBanner
			handshakeTimeout = desiredHandshake
			deliveryTimeout = desiredDelivery
		}
	} else {
		// No context deadline - use config defaults
		bannerTimeout = time.Duration(cfg.BannerTimeoutSeconds) * time.Second
		handshakeTimeout = time.Duration(cfg.HandshakeTimeoutSeconds) * time.Second
		deliveryTimeout = time.Duration(cfg.SMTPTimeoutSeconds) * time.Second
	}

	logger.Debug("attempting SMTP connection with phased timeouts",
		"mx", mxHost,
		"banner_timeout", bannerTimeout,
		"handshake_timeout", handshakeTimeout,
		"delivery_timeout", deliveryTimeout)

	// TLS configuration
	// For SMTP STARTTLS, use empty ServerName to disable SNI
	// Some SMTP servers have issues with SNI and reject the handshake
	tlsConfig := &tls.Config{
		ServerName:         "",   // Disable SNI for SMTP compatibility
		InsecureSkipVerify: true, // Opportunistic TLS
		MinVersion:         tls.VersionTLS12,
		CipherSuites:       opportunisticCipherSuites,
	}

	logger.Debug("TLS configuration",
		"server_name", "(disabled for SMTP)",
		"hello_hostname", cfg.HelloHostname,
		"mx_host", mxHost,
		"target_host", host,
		"min_version", "TLS1.2")

	// Perform SMTP handshake (EHLO) and optionally STARTTLS with phased timeout enforcement
	type clientResult struct {
		client  *smtp.Client
		conn    net.Conn      // raw connection for LMTP (bypasses go-smtp)
		reader  *bufio.Reader // buffered reader for LMTP (preserves state from handshake)
		usedTLS bool
		err     error
	}
	resultCh := make(chan clientResult, 1)

	recovery.SafeGo(d.logger, "smtp-handshake", func() {
		// Phase 1+2: Banner + Handshake (banner + EHLO/LHLO + STARTTLS) with combined timeout
		// Note: banner and handshake are combined because go-smtp doesn't expose individual control
		handshakePhaseTimeout := bannerTimeout + handshakeTimeout
		logger.Debug("phases 1+2: banner and handshake", "timeout", handshakePhaseTimeout, "protocol", protocol)
		conn.SetDeadline(time.Now().Add(handshakePhaseTimeout))
		defer conn.SetDeadline(time.Time{}) // Clear deadline after handshake

		// LMTP mode: raw protocol (go-smtp's LMTP client doesn't support per-recipient DATA responses)
		if protocol == config.ProtocolLMTP {
			var lmtpConn net.Conn = conn
			usedTLS := false

			// Implicit TLS for LMTP: wrap connection in TLS before LHLO
			if cfg.LMTPImplicitTLS {
				tlsConn := tls.Client(conn, tlsConfig)
				if err := tlsConn.HandshakeContext(ctx); err != nil {
					conn.Close()
					connClosed.Store(true)
					resultCh <- clientResult{err: fmt.Errorf("implicit TLS handshake for LMTP failed: %w", err)}
					return
				}
				lmtpConn = tlsConn
				usedTLS = true
			}

			reader := bufio.NewReader(lmtpConn)

			// Read server greeting (220)
			code, _, err := readLMTPResponse(reader, lmtpConn, handshakePhaseTimeout)
			if err != nil {
				lmtpConn.Close()
				connClosed.Store(true)
				resultCh <- clientResult{err: fmt.Errorf("LMTP greeting: %w", err)}
				return
			}
			if code != 220 {
				lmtpConn.Close()
				connClosed.Store(true)
				resultCh <- clientResult{err: fmt.Errorf("LMTP unexpected greeting code: %d", code)}
				return
			}

			// Send LHLO
			if err := writeLMTPCommand(lmtpConn, handshakePhaseTimeout, "LHLO %s", cfg.HelloHostname); err != nil {
				lmtpConn.Close()
				connClosed.Store(true)
				resultCh <- clientResult{err: fmt.Errorf("LHLO write: %w", err)}
				return
			}
			code, _, err = readLMTPResponse(reader, lmtpConn, handshakePhaseTimeout)
			if err != nil {
				lmtpConn.Close()
				connClosed.Store(true)
				resultCh <- clientResult{err: fmt.Errorf("LHLO response: %w", err)}
				return
			}
			if code != 250 {
				lmtpConn.Close()
				connClosed.Store(true)
				resultCh <- clientResult{err: fmt.Errorf("LHLO rejected: %d", code)}
				return
			}

			// Clear deadline
			lmtpConn.SetDeadline(time.Time{})
			logger.Info("LMTP connection established", "mx", mxHost, "hello_hostname", cfg.HelloHostname, "tls", usedTLS)
			resultCh <- clientResult{conn: lmtpConn, reader: reader, usedTLS: usedTLS, err: nil}
			return
		}

		// Implicit TLS (SMTPS): connection is TLS from the start (e.g. port 465)
		if cfg.SMTPImplicitTLS {
			tlsConn := tls.Client(conn, tlsConfig)
			if err := tlsConn.HandshakeContext(ctx); err != nil {
				conn.Close()
				connClosed.Store(true)
				resultCh <- clientResult{err: fmt.Errorf("implicit TLS handshake failed: %w", err)}
				return
			}
			// Create SMTP client on top of TLS connection (reads banner, sends EHLO)
			client := smtp.NewClient(tlsConn)
			if err := client.Hello(cfg.HelloHostname); err != nil {
				client.Close()
				connClosed.Store(true)
				resultCh <- clientResult{err: fmt.Errorf("EHLO failed over implicit TLS: %w", err)}
				return
			}
			tlsConn.SetDeadline(time.Time{})
			client.CommandTimeout = deliveryTimeout
			client.SubmissionTimeout = deliveryTimeout
			logger.Info("implicit TLS (SMTPS) connection established", "mx", mxHost, "hello_hostname", cfg.HelloHostname, "tls", true)
			resultCh <- clientResult{client: client, usedTLS: true, err: nil}
			return
		}

		// SMTP mode: Try STARTTLS first (opportunistic) - this does banner + EHLO + STARTTLS
		client, err := smtp.NewClientStartTLSWithName(conn, tlsConfig, cfg.HelloHostname)
		if err == nil {
			// STARTTLS succeeded - TLS handshake completed
			client.CommandTimeout = deliveryTimeout
			client.SubmissionTimeout = deliveryTimeout
			logger.Info("STARTTLS connection established", "mx", mxHost, "hello_hostname", cfg.HelloHostname)
			resultCh <- clientResult{client: client, usedTLS: true, err: nil}
			return
		}

		// STARTTLS failed - check if it's because server doesn't support it
		if strings.Contains(err.Error(), "server doesn't support STARTTLS") {
			// Server doesn't support STARTTLS - connection is now in bad state, need fresh connection
			logger.Warn("STARTTLS not supported, reconnecting for plaintext SMTP", "mx", mxHost)

			// Close corrupted connection and mark it so the deferred
			// cleanup in the outer function doesn't double-close.
			conn.Close()
			connClosed.Store(true)

			// Reconnect without STARTTLS (reuse same timeout for reconnect)
			dialer := &net.Dialer{Timeout: connectionTimeout}
			if sourceIP != "" {
				ip := net.ParseIP(sourceIP)
				dialer.LocalAddr = &net.TCPAddr{IP: ip}
			}

			// Reconnect to same target
			newConn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(targetIP, targetPort))
			if err != nil {
				resultCh <- clientResult{err: fmt.Errorf("reconnect for plaintext failed: %w", err)}
				return
			}

			// Set deadline on new connection for banner + EHLO
			newConn.SetDeadline(time.Now().Add(handshakePhaseTimeout))

			// Create plaintext client (reads banner)
			client := smtp.NewClient(newConn)
			// Send EHLO/HELO
			if err := client.Hello(cfg.HelloHostname); err != nil {
				client.Close()
				resultCh <- clientResult{err: fmt.Errorf("EHLO/HELO failed: %w", err)}
				return
			}

			// Clear deadline and set timeouts for delivery
			newConn.SetDeadline(time.Time{})
			client.CommandTimeout = deliveryTimeout
			client.SubmissionTimeout = deliveryTimeout
			logger.Info("plaintext SMTP connection established", "mx", mxHost, "hello_hostname", cfg.HelloHostname)
			resultCh <- clientResult{client: client, usedTLS: false, err: nil}
			return
		}

		// Other STARTTLS errors (handshake failed, etc.) - log detailed error
		logger.Error("STARTTLS handshake failed",
			"mx", mxHost,
			"error", err.Error(),
			"tls_min_version", "TLS1.2",
			"skip_verify", true)
		conn.Close()
		connClosed.Store(true)
		resultCh <- clientResult{err: fmt.Errorf("STARTTLS handshake failed: %w", err)}
	})

	select {
	case result := <-resultCh:
		if result.err != nil {
			// Goroutine always closes conn on error paths, so connClosed
			// is already true. Close any extra resources returned.
			if result.client != nil {
				result.client.Close()
			} else if result.conn != nil {
				result.conn.Close()
			}
			return nil, DeliveryResult{TraceID: traceID, Status: "temp_fail", MXHost: mxHost, SourceIP: sourceIP, Error: fmt.Sprintf("Handshake failed: %v", result.err)}, result.err
		}
		if !result.usedTLS && protocol != config.ProtocolLMTP {
			logger.Warn("delivering over plaintext SMTP (no TLS)", "mx", mxHost)
		}
		success = true // Prevent deferred close
		return &dialResult{
			Client: result.client,
			Conn:   result.conn,
			Reader: result.reader,
		}, DeliveryResult{}, nil
	case <-ctx.Done():
		// The goroutine may still be running and blocked on conn I/O.
		// Close conn to unblock it. This may double-close if the goroutine
		// already closed conn, but net.TCPConn.Close on an already-closed
		// connection just returns an error without side effects.
		conn.Close()
		connClosed.Store(true)
		return nil, DeliveryResult{TraceID: traceID, Status: "timeout", MXHost: mxHost, SourceIP: sourceIP, Error: "SMTP handshake timed out"}, ctx.Err()
	}
}

func (d *Deliverer) mapSMTPError(logger *slog.Logger, traceID string, err error, mxHost, sourceIP string) DeliveryResult {
	res := DeliveryResult{TraceID: traceID, MXHost: mxHost, SourceIP: sourceIP}

	// Extract SMTP code and message first if available
	var smtpCode int
	var smtpMessage string
	if smtpErr, ok := err.(*smtp.SMTPError); ok {
		smtpCode = smtpErr.Code
		// Normalize multi-line SMTP messages by replacing newlines with spaces
		smtpMessage = strings.ReplaceAll(smtpErr.Message, "\n", " ")
		res.SMTPCode = smtpCode
		res.SMTPMessage = smtpMessage
	}

	// Classify using our error classifier with actual SMTP code/message
	classified := ClassifyError(smtpCode, smtpMessage, err)
	res.Error = classified.Message

	logger.Debug("classifying SMTP error",
		"mx", mxHost,
		"error", err,
		"smtp_code", smtpCode,
		"category", classified.Category,
		"retryable", IsRetryable(classified.Category))

	// Map category to status
	if smtpCode > 0 {
		// SMTP error - use code-based classification
		if smtpCode >= 500 {
			res.Status = "hard_bounce"
		} else {
			res.Status = "temp_fail"
		}
	} else {
		// Non-SMTP error - use category-based mapping
		switch classified.Category {
		case ErrorPermanent:
			res.Status = "hard_bounce"
		case ErrorTemporary, ErrorGreylist, ErrorThrottled:
			res.Status = "temp_fail"
		case ErrorNetwork:
			res.Status = "timeout"
		case ErrorReputation:
			res.Status = "temp_fail" // Let reputation tracker handle degradation
		default:
			res.Status = "error"
		}

		// Specific check for bind errors which should be "error" status
		if isBindError(err) {
			res.Status = "error"
		}
	}
	return res
}

func (d *Deliverer) recordMetrics(result DeliveryResult, recipientDomain string) {
	if d.metrics != nil {
		d.metrics.RecordDeliveryAttempt(result.Status, recipientDomain, float64(result.AttemptDurationMs)/1000.0)
	}
}

// IPRotator logic - supports IPv4/IPv6 separation and selection
type IPRotator struct {
	ipsV4     []string
	ipsV6     []string
	strategy  string
	counterV4 uint32
	counterV6 uint32
	random    *rand.Rand
	randomMu  sync.Mutex // Protects random for thread-safe access
}

// NewIPRotator creates a new IP rotator with separate IPv4 and IPv6 pools.
func NewIPRotator(ipsV4, ipsV6 []string, strategy string) *IPRotator {
	return &IPRotator{
		ipsV4:    ipsV4,
		ipsV6:    ipsV6,
		strategy: strategy,
		random:   rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

// SelectIPs returns an ordered list of IPs to try based on the rotation strategy.
func (r *IPRotator) SelectIPs(useIPv6 bool, domain string) []string {
	ips := r.ipsV4
	counter := &r.counterV4
	if useIPv6 {
		ips = r.ipsV6
		counter = &r.counterV6
	}

	if len(ips) == 0 {
		return nil
	}

	if len(ips) == 1 {
		return []string{ips[0]}
	}

	var startIndex int
	switch r.strategy {
	case "random":
		r.randomMu.Lock()
		startIndex = r.random.Intn(len(ips))
		r.randomMu.Unlock()
	case "hash-domain":
		h := fnv.New32a()
		h.Write([]byte(domain))
		startIndex = int(h.Sum32() % uint32(len(ips)))
	case "round-robin":
		fallthrough
	default:
		c := atomic.AddUint32(counter, 1)
		startIndex = int((c - 1) % uint32(len(ips)))
	}

	// Create a rotated copy starting from startIndex
	result := make([]string, len(ips))
	for i := 0; i < len(ips); i++ {
		result[i] = ips[(startIndex+i)%len(ips)]
	}
	return result
}

// RandomIntn returns a random integer in [0, n) with thread-safe access.
func (r *IPRotator) RandomIntn(n int) int {
	r.randomMu.Lock()
	defer r.randomMu.Unlock()
	return r.random.Intn(n)
}

// GetAllIPsV4 returns all IPv4 addresses.
func (r *IPRotator) GetAllIPsV4() []string {
	return r.ipsV4
}

// GetAllIPsV6 returns all IPv6 addresses.
func (r *IPRotator) GetAllIPsV6() []string {
	return r.ipsV6
}

// HasIPv4 returns true if IPv4 addresses are available.
func (r *IPRotator) HasIPv4() bool {
	return len(r.ipsV4) > 0
}

// HasIPv6 returns true if IPv6 addresses are available.
func (r *IPRotator) HasIPv6() bool {
	return len(r.ipsV6) > 0
}

func splitEmail(email string) (string, string) {
	i := strings.LastIndexByte(email, '@')
	if i < 0 {
		return "", ""
	}
	return email[:i], email[i+1:]
}

// stripAngleBrackets removes one surrounding pair of <> from an envelope
// address. "" and "<>" both collapse to "" (the null sender).
func stripAngleBrackets(addr string) string {
	if len(addr) >= 2 && addr[0] == '<' && addr[len(addr)-1] == '>' {
		return addr[1 : len(addr)-1]
	}
	return addr
}

// isBindError checks if an error is related to binding to a local address.
// This typically indicates misconfiguration (IP not assigned to interface).
func isBindError(err error) bool {
	if err == nil {
		return false
	}
	// Check for common bind-related error messages
	errStr := err.Error()
	return strings.Contains(errStr, "bind") ||
		strings.Contains(errStr, "cannot assign requested address") ||
		strings.Contains(errStr, "EADDRNOTAVAIL")
}

// SetMetrics sets the metrics recorder
func (d *Deliverer) SetMetrics(metrics DeliveryMetrics) {
	d.metrics = metrics
}

// logDeliveryResult logs the final delivery result at INFO level
func (d *Deliverer) logDeliveryResult(logger *slog.Logger, from, to string, result DeliveryResult) {
	logger.Info("delivery completed",
		"from", from,
		"to", to,
		"status", result.Status,
		"smtp_code", result.SMTPCode,
		"smtp_message", result.SMTPMessage,
		"mx_host", result.MXHost,
		"source_ip", result.SourceIP,
		"duration_ms", result.AttemptDurationMs,
		"error", result.Error)
}

// needsUTF8Address checks if an email address contains non-ASCII characters.
func needsUTF8Address(email string) bool {
	for _, r := range email {
		if r > 127 {
			return true
		}
	}
	return false
}

// extractHeaders returns the RFC 822 headers from a message (up to the first blank line),
// truncated to 2KB for logging. Used for debug diagnostics.
func extractHeaders(msg []byte) string {
	const maxLen = 2048
	// Find the header/body separator: \r\n\r\n or \n\n
	end := bytes.Index(msg, []byte("\r\n\r\n"))
	if end < 0 {
		end = bytes.Index(msg, []byte("\n\n"))
	}
	if end < 0 {
		end = len(msg)
	}
	if end > maxLen {
		end = maxLen
	}
	return string(msg[:end])
}

// supportsExtension checks if the SMTP server supports a given extension.
func supportsExtension(client *smtp.Client, ext string) bool {
	if client == nil {
		return false
	}
	supported, _ := client.Extension(ext)
	return supported
}

// isTimeoutError checks if an error is a network timeout (i/o timeout, deadline exceeded).
func isTimeoutError(err error) bool {
	if err == nil {
		return false
	}
	if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
		return true
	}
	return false
}

// lmtpStatus returns "timeout" if err is a network timeout, otherwise "error".
func lmtpStatus(err error) string {
	if isTimeoutError(err) {
		return "timeout"
	}
	return "error"
}

// --- LMTP raw protocol implementation (mirrors Kanal's approach) ---
// go-smtp's LMTP client doesn't support per-recipient DATA responses,
// so we implement the protocol directly on net.Conn.

// performLMTPTransaction sends MAIL FROM, RCPT TO, DATA, and reads the
// per-recipient response over a raw connection. The connection is always
// closed when done (no pooling for LMTP).
func (d *Deliverer) performLMTPTransaction(ctx context.Context, logger *slog.Logger, traceID string, conn net.Conn, reader *bufio.Reader, from, to string, msg []byte, mxHost, sourceIP string) DeliveryResult {
	defer func() {
		// Best-effort QUIT before closing (avoids Dovecot warnings)
		conn.SetWriteDeadline(time.Now().Add(1 * time.Second))
		fmt.Fprintf(conn, "QUIT\r\n")
		conn.Close()
	}()

	cfg := d.getConfig()
	cmdTimeout := time.Duration(cfg.LMTPTimeoutSeconds) * time.Second
	if cmdTimeout == 0 {
		cmdTimeout = 60 * time.Second
	}

	// Accept callers that pass envelope addresses with or without angle brackets.
	// Null sender ("" or "<>") both produce MAIL FROM:<>.
	from = stripAngleBrackets(from)
	to = stripAngleBrackets(to)

	// Log envelope and first message headers at DEBUG for diagnostics
	if logger.Enabled(ctx, slog.LevelDebug) {
		logger.Debug("LMTP envelope",
			"mail_from", from,
			"rcpt_to", to,
			"message_headers", extractHeaders(msg))
	}

	// MAIL FROM
	if err := writeLMTPCommand(conn, cmdTimeout, "MAIL FROM:<%s>", from); err != nil {
		return DeliveryResult{TraceID: traceID, Status: lmtpStatus(err), MXHost: mxHost, SourceIP: sourceIP, Error: fmt.Sprintf("MAIL FROM write: %v", err)}
	}
	code, msg2, err := readLMTPResponse(reader, conn, cmdTimeout)
	if err != nil {
		return DeliveryResult{TraceID: traceID, Status: lmtpStatus(err), MXHost: mxHost, SourceIP: sourceIP, Error: fmt.Sprintf("MAIL FROM response: %v", err)}
	}
	if code != 250 {
		return classifyLMTPResult(traceID, mxHost, sourceIP, code, msg2)
	}

	// RCPT TO
	if err := writeLMTPCommand(conn, cmdTimeout, "RCPT TO:<%s>", to); err != nil {
		return DeliveryResult{TraceID: traceID, Status: lmtpStatus(err), MXHost: mxHost, SourceIP: sourceIP, Error: fmt.Sprintf("RCPT TO write: %v", err)}
	}
	code, msg2, err = readLMTPResponse(reader, conn, cmdTimeout)
	if err != nil {
		return DeliveryResult{TraceID: traceID, Status: lmtpStatus(err), MXHost: mxHost, SourceIP: sourceIP, Error: fmt.Sprintf("RCPT TO response: %v", err)}
	}
	if code != 250 {
		return classifyLMTPResult(traceID, mxHost, sourceIP, code, msg2)
	}

	// DATA
	if err := writeLMTPCommand(conn, cmdTimeout, "DATA"); err != nil {
		return DeliveryResult{TraceID: traceID, Status: lmtpStatus(err), MXHost: mxHost, SourceIP: sourceIP, Error: fmt.Sprintf("DATA write: %v", err)}
	}
	code, msg2, err = readLMTPResponse(reader, conn, cmdTimeout)
	if err != nil {
		return DeliveryResult{TraceID: traceID, Status: lmtpStatus(err), MXHost: mxHost, SourceIP: sourceIP, Error: fmt.Sprintf("DATA response: %v", err)}
	}
	if code != 354 {
		return classifyLMTPResult(traceID, mxHost, sourceIP, code, msg2)
	}

	// Send message body with dot-stuffing
	if err := writeLMTPData(conn, cmdTimeout, msg); err != nil {
		return DeliveryResult{TraceID: traceID, Status: lmtpStatus(err), MXHost: mxHost, SourceIP: sourceIP, Error: fmt.Sprintf("DATA body write: %v", err)}
	}

	// LMTP: read one status per RCPT TO (we send exactly one recipient)
	code, msg2, err = readLMTPResponse(reader, conn, cmdTimeout)
	if err != nil {
		return DeliveryResult{TraceID: traceID, Status: lmtpStatus(err), MXHost: mxHost, SourceIP: sourceIP, Error: fmt.Sprintf("DATA final response: %v", err)}
	}

	result := classifyLMTPResult(traceID, mxHost, sourceIP, code, msg2)
	logger.Debug("LMTP transaction complete", "code", code, "message", msg2, "status", result.Status)
	return result
}

// classifyLMTPResult maps an LMTP response code to a DeliveryResult.
func classifyLMTPResult(traceID, mxHost, sourceIP string, code int, msg string) DeliveryResult {
	result := DeliveryResult{
		TraceID:     traceID,
		MXHost:      mxHost,
		SourceIP:    sourceIP,
		SMTPCode:    code,
		SMTPMessage: msg,
	}
	switch {
	case code >= 200 && code < 300:
		result.Status = "delivered"
	case code >= 400 && code < 500:
		result.Status = "temp_fail"
	case code >= 500 && code < 600:
		result.Status = "hard_bounce"
	default:
		result.Status = "error"
		result.Error = fmt.Sprintf("unexpected LMTP code: %d", code)
	}
	return result
}

// writeLMTPCommand sends a command line over the connection with a timeout.
func writeLMTPCommand(conn net.Conn, timeout time.Duration, format string, args ...interface{}) error {
	if err := conn.SetWriteDeadline(time.Now().Add(timeout)); err != nil {
		return fmt.Errorf("set write deadline: %w", err)
	}
	cmd := fmt.Sprintf(format, args...)
	_, err := fmt.Fprintf(conn, "%s\r\n", cmd)
	return err
}

// writeLMTPData sends the message body with dot-stuffing and the terminating dot.
// Uses a direct []byte walk instead of bufio.Scanner to avoid per-line string allocations.
func writeLMTPData(conn net.Conn, timeout time.Duration, message []byte) error {
	if err := conn.SetWriteDeadline(time.Now().Add(timeout)); err != nil {
		return fmt.Errorf("set write deadline: %w", err)
	}

	writer := bufio.NewWriterSize(conn, 64*1024)
	remaining := message

	for len(remaining) > 0 {
		i := bytes.IndexByte(remaining, '\n')
		var line []byte
		if i < 0 {
			line = remaining
			remaining = nil
		} else {
			line = remaining[:i]
			remaining = remaining[i+1:]
		}
		// Strip trailing CR (CRLF → just use our own CRLF on output)
		line = bytes.TrimSuffix(line, []byte{'\r'})
		// Dot-stuff lines starting with '.'
		if len(line) > 0 && line[0] == '.' {
			if err := writer.WriteByte('.'); err != nil {
				return err
			}
		}
		if _, err := writer.Write(line); err != nil {
			return err
		}
		if _, err := writer.WriteString("\r\n"); err != nil {
			return err
		}
	}

	// Terminating dot
	if _, err := writer.WriteString(".\r\n"); err != nil {
		return err
	}
	return writer.Flush()
}

// readLMTPResponse reads a (possibly multi-line) LMTP response.
func readLMTPResponse(reader *bufio.Reader, conn net.Conn, timeout time.Duration) (int, string, error) {
	if err := conn.SetReadDeadline(time.Now().Add(timeout)); err != nil {
		return 0, "", fmt.Errorf("set read deadline: %w", err)
	}

	var lines []string
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				return 0, "", fmt.Errorf("connection closed by server")
			}
			return 0, "", err
		}

		line = strings.TrimRight(line, "\r\n")
		if len(line) < 3 {
			return 0, "", fmt.Errorf("invalid response line: %q", line)
		}

		code, err := strconv.Atoi(line[:3])
		if err != nil {
			return 0, "", fmt.Errorf("invalid response code in: %q", line)
		}

		if len(line) > 4 {
			lines = append(lines, line[4:])
		}

		// Multi-line: '-' at position 3; final line: ' ' or end
		if len(line) == 3 || line[3] == ' ' {
			return code, strings.Join(lines, " "), nil
		}
	}
}
