package delivery

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"hash/fnv"
	"log/slog"
	"math/rand"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"fune/internal/arc"
	"fune/internal/config"
	"fune/internal/dkim"
	"fune/internal/recovery"
	"fune/internal/srs"

	"github.com/emersion/go-smtp"
)

// ErrDomainRateLimitExceeded is returned when the domain rate limit is exceeded (Fail Fast).
var ErrDomainRateLimitExceeded = fmt.Errorf("domain rate limit exceeded")

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
	RecordDeliveryAttempt(outcome string, duration float64)
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
		ipRotator:         NewIPRotator(ipsV4, ipsV6, config.SourceIPSelection, config.PreferIPv6),
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

// GetConnectionPool returns the connection pool (for testing/inspection)
func (d *Deliverer) GetConnectionPool() *ConnectionPool {
	return d.pool
}

// GetReputationTracker returns the IP reputation tracker (for testing/inspection)
func (d *Deliverer) GetReputationTracker() *IPReputationTracker {
	return d.reputationTracker
}

// DeliverMessage attempts to deliver a message synchronously.
func (d *Deliverer) DeliverMessage(ctx context.Context, from, to string, message []byte, dkimPrivateKey, dkimSelector, dkimDomain string, skipDKIMValidation bool, arcPrivateKey, arcSelector, arcDomain string, inboundAuth *InboundAuthResults) DeliveryResult {
	start := time.Now()

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
	if d.config.PerDomainIntervalSeconds > 0 && !d.isDomainWhitelisted(domain) {
		if err := d.waitForDomainRateLimit(ctx, domain); err != nil {
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
		logger.Debug("skipping ARC signing - missing parameters",
			"has_private_key", finalARCPrivateKey != "",
			"has_selector", finalARCSelector != "",
			"has_domain", finalARCDomain != "")
	}

	// 4. Lookup MX records
	logger.Debug("looking up MX records", "domain", domain)
	mxRecords, err := d.mxLookup.Lookup(ctx, domain)
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

	// 5. Try each MX with IPv6/IPv4 preference
	var lastResult DeliveryResult

	// Determine delivery order: IPv6 first or IPv4 first
	tryIPv6First := d.ipRotator.PreferIPv6() && d.ipRotator.HasIPv6()
	tryIPv4 := d.ipRotator.HasIPv4()
	tryIPv6 := d.ipRotator.HasIPv6()

	for i, mx := range mxRecords {
		logger.Info("trying MX", "host", mx.Host, "priority", mx.Priority, "index", i, "total", len(mxRecords))

		// Check context
		if ctx.Err() != nil {
			logger.Debug("context canceled during MX attempts", "error", ctx.Err())
			lastResult = DeliveryResult{TraceID: traceID, Status: "timeout", Error: "Context canceled", MXHost: mx.Host}
			break
		}

		// Try IPv6 first if preferred
		if tryIPv6First {
			logger.Debug("trying IPv6 first", "mx", mx.Host)
			result := d.tryDeliveryWithIPVersion(ctx, logger, traceID, from, to, signedMessage, mx.Host, true, start, inboundAuth)
			// Return immediately for definitive results (don't try other MX servers)
			if result.Status == "delivered" || result.Status == "hard_bounce" || result.Status == "temp_fail" {
				result.AttemptDurationMs = time.Since(start).Milliseconds()
				d.recordMetrics(result)
				d.logDeliveryResult(logger, from, to, result)
				return result
			}
			lastResult = result

			// Fall back to IPv4 if IPv6 failed and IPv4 is available
			if tryIPv4 {
				logger.Debug("falling back to IPv4", "mx", mx.Host)
				result = d.tryDeliveryWithIPVersion(ctx, logger, traceID, from, to, signedMessage, mx.Host, false, start, inboundAuth)
				if result.Status == "delivered" || result.Status == "hard_bounce" || result.Status == "temp_fail" {
					result.AttemptDurationMs = time.Since(start).Milliseconds()
					d.recordMetrics(result)
					d.logDeliveryResult(logger, from, to, result)
					return result
				}
				lastResult = result
			}
		} else if tryIPv4 {
			// Try IPv4 first (or only)
			logger.Debug("trying IPv4", "mx", mx.Host)
			result := d.tryDeliveryWithIPVersion(ctx, logger, traceID, from, to, signedMessage, mx.Host, false, start, inboundAuth)
			if result.Status == "delivered" || result.Status == "hard_bounce" || result.Status == "temp_fail" {
				result.AttemptDurationMs = time.Since(start).Milliseconds()
				d.recordMetrics(result)
				d.logDeliveryResult(logger, from, to, result)
				return result
			}
			lastResult = result

			// Fall back to IPv6 if IPv4 failed and IPv6 is available
			if tryIPv6 {
				logger.Debug("falling back to IPv6", "mx", mx.Host)
				result = d.tryDeliveryWithIPVersion(ctx, logger, traceID, from, to, signedMessage, mx.Host, true, start, inboundAuth)
				if result.Status == "delivered" || result.Status == "hard_bounce" || result.Status == "temp_fail" {
					result.AttemptDurationMs = time.Since(start).Milliseconds()
					d.recordMetrics(result)
					d.logDeliveryResult(logger, from, to, result)
					return result
				}
				lastResult = result
			}
		} else {
			// No source IPs configured - use system default
			logger.Debug("no source IPs configured, using system default", "mx", mx.Host)
			result := d.attemptDelivery(ctx, logger, traceID, from, to, signedMessage, mx.Host, "", true, inboundAuth)
			deliveryInfo := DeliveryInfo{From: from, To: to, MXHost: mx.Host}
			d.reputationTracker.RecordDeliveryAttempt("", result.Status == "delivered", nil, deliveryInfo)
			if result.Status == "delivered" || result.Status == "hard_bounce" || result.Status == "temp_fail" {
				result.AttemptDurationMs = time.Since(start).Milliseconds()
				d.recordMetrics(result)
				d.logDeliveryResult(logger, from, to, result)
				return result
			}
			lastResult = result
		}

		// For timeout/error results, continue to next MX server
		// (delivered/hard_bounce/temp_fail already returned above)
	}

	lastResult.AttemptDurationMs = time.Since(start).Milliseconds()
	d.recordMetrics(lastResult)
	d.logDeliveryResult(logger, from, to, lastResult)
	return lastResult
}

// tryDeliveryWithIPVersion attempts delivery using either IPv6 or IPv4 source IPs and matching MX host IPs.
func (d *Deliverer) tryDeliveryWithIPVersion(ctx context.Context, logger *slog.Logger, traceID string, from, to string, message []byte, mxHost string, useIPv6 bool, startTime time.Time, inboundAuth *InboundAuthResults) DeliveryResult {
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

		result := d.attemptDelivery(ctx, logger, traceID, from, to, message, mxHost, sourceIP, useIPv6, inboundAuth)
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
func (d *Deliverer) isDomainWhitelisted(domain string) bool {
	if len(d.config.RateLimitWhitelist) == 0 {
		return false
	}

	// Case-insensitive domain matching
	domainLower := strings.ToLower(domain)
	for _, whitelistedDomain := range d.config.RateLimitWhitelist {
		if strings.ToLower(whitelistedDomain) == domainLower {
			d.logger.Debug("domain is whitelisted from rate limiting", "domain", domain)
			return true
		}
	}
	return false
}

func (d *Deliverer) waitForDomainRateLimit(ctx context.Context, domain string) error {
	initialTokens := float64(d.config.PerDomainBurst)
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
	interval := time.Duration(d.config.PerDomainIntervalSeconds) * time.Second
	burst := float64(d.config.PerDomainBurst)
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

func (d *Deliverer) attemptDelivery(ctx context.Context, logger *slog.Logger, traceID string, from, to string, msg []byte, mxHost, sourceIP string, preferIPv6 bool, inboundAuth *InboundAuthResults) DeliveryResult {
	// 1. Try to get a pooled connection
	var client *smtp.Client
	if d.pool != nil {
		client = d.pool.Get(mxHost, sourceIP)
		if client != nil {
			logger.Debug("using pooled connection", "mx", mxHost)

			result := d.performDeliveryTransaction(logger, traceID, client, from, to, msg, mxHost, sourceIP, true, inboundAuth)
			if result.Status == "delivered" || result.Status == "hard_bounce" || result.Status == "timeout" {
				return result
			}

			// If failed and was reused, try once more with a fresh connection
			logger.Debug("pooled connection failed, attempting with fresh connection",
				"mx", mxHost,
				"error", result.Error)
			client = nil
		}
	}

	// 2. If no pooled connection (or reused failed), dial a new one
	if client == nil {
		var err error
		var result DeliveryResult
		client, result, err = d.dialAndHello(ctx, logger, traceID, mxHost, sourceIP, preferIPv6)
		if err != nil {
			return result
		}
	}

	// 3. Deliver message logic
	return d.performDeliveryTransaction(logger, traceID, client, from, to, msg, mxHost, sourceIP, false, inboundAuth)
}

func (d *Deliverer) performDeliveryTransaction(logger *slog.Logger, traceID string, client *smtp.Client, from, to string, msg []byte, mxHost, sourceIP string, reused bool, inboundAuth *InboundAuthResults) DeliveryResult {
	logger.Debug("starting delivery transaction",
		"mx", mxHost,
		"source_ip", sourceIP,
		"reused", reused)

	smtpMsg, err := d.deliverPayload(logger, client, from, to, msg, inboundAuth)
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

func (d *Deliverer) deliverPayload(logger *slog.Logger, client *smtp.Client, from, to string, msg []byte, inboundAuth *InboundAuthResults) (string, error) {
	// MAIL FROM
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

	logger.Debug("sending MAIL FROM", "from", mailFrom)
	if err := client.Mail(mailFrom, mailOpts); err != nil {
		logger.Debug("MAIL FROM failed", "error", err)
		return "", err
	}

	// RCPT TO
	logger.Debug("sending RCPT TO", "to", to)
	if err := client.Rcpt(to, nil); err != nil {
		logger.Debug("RCPT TO failed", "error", err)
		return "", err
	}

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

	logger.Debug("delivery transaction successful", "smtp_response", resp.StatusText)
	return resp.StatusText, nil
}

func (d *Deliverer) dialAndHello(ctx context.Context, logger *slog.Logger, traceID string, mxHost, sourceIP string, preferIPv6 bool) (*smtp.Client, DeliveryResult, error) {
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

	// Fallback if no matching IP version found
	if targetIP == "" {
		if len(mxIPs) > 0 {
			targetIP = mxIPs[0]
			logger.Debug("no matching IP version for MX, using first available",
				"mx", mxHost,
				"require_ipv6", isSourceIPv6,
				"target_ip", targetIP)
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

	// TCP connection to port 25 (SMTP)
	// Use the minimum of configured timeout and remaining context deadline
	connectionTimeout := time.Duration(d.config.ConnectionTimeoutSeconds) * time.Second
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

		// Ensure target IP version matches source IP version to avoid bind errors
		isTargetIPv6 := net.ParseIP(targetIP).To4() == nil
		if isSourceIPv6 != isTargetIPv6 {
			return nil, DeliveryResult{
				TraceID:  traceID,
				Status:   "error",
				MXHost:   mxHost,
				SourceIP: sourceIP,
				Error:    fmt.Sprintf("IP version mismatch: source %s (v6=%v) cannot connect to target %s (v6=%v)", sourceIP, isSourceIPv6, targetIP, isTargetIPv6),
			}, fmt.Errorf("IP version mismatch")
		}

		dialer.LocalAddr = &net.TCPAddr{IP: ip}
	}

	logger.Info("connecting to MX", "mx", mxHost, "target_ip", targetIP, "source_ip", sourceIP)

	// Connect to the target IP
	conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(targetIP, "25"))
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

	// Ensure connection is closed if setup fails
	success := false
	defer func() {
		if !success {
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

	// Calculate remaining timeout from context
	var commandTimeout time.Duration
	if deadline, ok := ctx.Deadline(); ok {
		commandTimeout = time.Until(deadline)
		if commandTimeout <= 0 {
			conn.Close()
			return nil, DeliveryResult{Status: "timeout", MXHost: mxHost, SourceIP: sourceIP, Error: "context deadline exceeded"}, ctx.Err()
		}
	} else {
		// Default timeout if no context deadline
		commandTimeout = time.Duration(d.config.SMTPTimeoutSeconds) * time.Second
	}

	// TLS configuration
	tlsConfig := &tls.Config{
		ServerName:         host,
		InsecureSkipVerify: true, // Opportunistic TLS
		MinVersion:         tls.VersionTLS12,
	}

	logger.Debug("attempting SMTP connection", "mx", mxHost, "timeout", commandTimeout)

	// Perform SMTP handshake (EHLO) and optionally STARTTLS with timeout enforcement using goroutine
	type clientResult struct {
		client  *smtp.Client
		usedTLS bool
		err     error
	}
	resultCh := make(chan clientResult, 1)

	recovery.SafeGo(d.logger, "smtp-handshake", func() {
		// Set deadline for the entire handshake
		conn.SetDeadline(time.Now().Add(commandTimeout))
		defer conn.SetDeadline(time.Time{}) // Clear deadline after handshake

		// Try STARTTLS first (opportunistic)
		client, err := smtp.NewClientStartTLSWithName(conn, tlsConfig, d.config.HelloHostname)
		if err == nil {
			// STARTTLS succeeded
			client.CommandTimeout = commandTimeout
			client.SubmissionTimeout = commandTimeout
			logger.Info("STARTTLS connection established", "mx", mxHost, "hello_hostname", d.config.HelloHostname)
			resultCh <- clientResult{client: client, usedTLS: true, err: nil}
			return
		}

		// STARTTLS failed - check if it's because server doesn't support it
		if strings.Contains(err.Error(), "server doesn't support STARTTLS") {
			// Server doesn't support STARTTLS - connection is now in bad state, need fresh connection
			logger.Warn("STARTTLS not supported, reconnecting for plaintext SMTP", "mx", mxHost)

			// Close corrupted connection
			conn.Close()

			// Reconnect without STARTTLS
			dialer := &net.Dialer{Timeout: connectionTimeout}
			if sourceIP != "" {
				ip := net.ParseIP(sourceIP)
				dialer.LocalAddr = &net.TCPAddr{IP: ip}
			}

			// Reconnect to same target
			newConn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(targetIP, "25"))
			if err != nil {
				resultCh <- clientResult{err: fmt.Errorf("reconnect for plaintext failed: %w", err)}
				return
			}

			// Set deadline on new connection
			newConn.SetDeadline(time.Now().Add(commandTimeout))

			// Create plaintext client
			client := smtp.NewClient(newConn)
			if err := client.Hello(d.config.HelloHostname); err != nil {
				client.Close()
				resultCh <- clientResult{err: fmt.Errorf("EHLO/HELO failed: %w", err)}
				return
			}

			client.CommandTimeout = commandTimeout
			client.SubmissionTimeout = commandTimeout
			logger.Info("plaintext SMTP connection established", "mx", mxHost, "hello_hostname", d.config.HelloHostname)
			resultCh <- clientResult{client: client, usedTLS: false, err: nil}
			return
		}

		// Other STARTTLS errors (handshake failed, etc.) - fail
		resultCh <- clientResult{err: fmt.Errorf("STARTTLS handshake failed: %w", err)}
	})

	var client *smtp.Client
	select {
	case result := <-resultCh:
		if result.err != nil {
			if result.client != nil {
				result.client.Close()
			} else {
				conn.Close()
			}
			return nil, DeliveryResult{TraceID: traceID, Status: "temp_fail", MXHost: mxHost, SourceIP: sourceIP, Error: fmt.Sprintf("Handshake failed: %v", result.err)}, result.err
		}
		client = result.client
		if !result.usedTLS {
			logger.Warn("delivering over plaintext SMTP (no TLS)", "mx", mxHost)
		}
	case <-ctx.Done():
		// Timeout occurred
		conn.Close()
		return nil, DeliveryResult{TraceID: traceID, Status: "timeout", MXHost: mxHost, SourceIP: sourceIP, Error: "SMTP handshake timed out"}, ctx.Err()
	}

	success = true // Prevent deferred close
	return client, DeliveryResult{}, nil
}

func (d *Deliverer) mapSMTPError(logger *slog.Logger, traceID string, err error, mxHost, sourceIP string) DeliveryResult {
	res := DeliveryResult{TraceID: traceID, MXHost: mxHost, SourceIP: sourceIP}

	// Extract SMTP code and message first if available
	var smtpCode int
	var smtpMessage string
	if smtpErr, ok := err.(*smtp.SMTPError); ok {
		smtpCode = smtpErr.Code
		smtpMessage = smtpErr.Message
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

func (d *Deliverer) recordMetrics(result DeliveryResult) {
	if d.metrics != nil {
		d.metrics.RecordDeliveryAttempt(result.Status, float64(result.AttemptDurationMs)/1000.0)
	}
}

// IPRotator logic - supports IPv4/IPv6 separation and selection
type IPRotator struct {
	ipsV4      []string
	ipsV6      []string
	strategy   string
	counterV4  uint32
	counterV6  uint32
	random     *rand.Rand
	randomMu   sync.Mutex // Protects random for thread-safe access
	preferIPv6 bool
}

// NewIPRotator creates a new IP rotator with separate IPv4 and IPv6 pools.
func NewIPRotator(ipsV4, ipsV6 []string, strategy string, preferIPv6 bool) *IPRotator {
	return &IPRotator{
		ipsV4:      ipsV4,
		ipsV6:      ipsV6,
		strategy:   strategy,
		random:     rand.New(rand.NewSource(time.Now().UnixNano())),
		preferIPv6: preferIPv6,
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

// PreferIPv6 returns whether IPv6 is preferred.
func (r *IPRotator) PreferIPv6() bool {
	return r.preferIPv6
}

func splitEmail(email string) (string, string) {
	i := bytes.LastIndexByte([]byte(email), '@')
	if i < 0 {
		return "", ""
	}
	return email[:i], email[i+1:]
}

// isBindError checks if an error is related to binding to a local address.
// This typically indicates misconfiguration (IP not assigned to interface).
func isBindError(err error) bool {
	if err == nil {
		return false
	}
	// Check for common bind-related error messages
	errStr := err.Error()
	return bytes.Contains([]byte(errStr), []byte("bind")) ||
		bytes.Contains([]byte(errStr), []byte("cannot assign requested address")) ||
		bytes.Contains([]byte(errStr), []byte("EADDRNOTAVAIL"))
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

// supportsExtension checks if the SMTP server supports a given extension.
func supportsExtension(client *smtp.Client, ext string) bool {
	if client == nil {
		return false
	}
	supported, _ := client.Extension(ext)
	return supported
}
