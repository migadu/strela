package delivery

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"math/rand"
	"net"
	"os"
	"sync"
	"time"

	"fune/internal/config"
	"fune/internal/recovery"
	"fune/internal/srs"

	"github.com/emersion/go-smtp"
)

// ErrDomainRateLimitExceeded is returned when the domain rate limit is exceeded (Fail Fast).
var ErrDomainRateLimitExceeded = fmt.Errorf("domain rate limit exceeded")

// DeliveryResult contains the complete result of a delivery attempt.
type DeliveryResult struct {
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
		var err error
		srsInstance, err = srs.NewSRS(
			srsConfig.Domain,
			srsConfig.Secret,
			srsConfig.MaxAge,
			srsConfig.HashLength,
			srsConfig.Separator,
			srsConfig.AlwaysRewrite,
		)
		if err != nil {
			logger.Error("failed to initialize SRS", "error", err)
			srsInstance = nil
		}
	}

	ipsV4 := expandedIPs.IPv4
	ipsV6 := expandedIPs.IPv6

	return &Deliverer{
		config:            config,
		arcConfig:         arcConfig,
		mxLookup:          mxLookup,
		logger:            logger,
		ipRotator:         NewIPRotator(ipsV4, ipsV6, config.SourceIPSelection, config.PreferIPv6),
		reputationTracker: reputationTracker,
		arcPrivateKey:     arcPrivateKey,
		srs:               srsInstance,
		pool:              NewConnectionPool(config.ConnectionPoolTTLSeconds),
	}
}

// DeliverMessage attempts to deliver a message synchronously.
func (d *Deliverer) DeliverMessage(ctx context.Context, from, to string, message []byte) DeliveryResult {
	start := time.Now()

	// Extract domain from recipient
	_, domain := splitEmail(to)
	if domain == "" {
		return DeliveryResult{
			Status: "hard_bounce",
			Error:  "Invalid recipient address",
		}
	}

	// 1. Wait for per-domain rate limit
	if err := d.waitForDomainRateLimit(ctx, domain); err != nil {
		if err == ErrDomainRateLimitExceeded {
			return DeliveryResult{
				Status: "rate_limit", // Fail Fast status
				Error:  "Domain rate limit exceeded",
			}
		}
		return DeliveryResult{
			Status: "timeout",
			Error:  "Rate limit check failed",
		}
	}

	// 2. Lookup MX records
	mxRecords, err := d.mxLookup.Lookup(ctx, domain)
	if err != nil {
		return DeliveryResult{
			Status: "temp_fail",
			Error:  fmt.Sprintf("MX lookup failed: %v", err),
		}
	}
	if len(mxRecords) == 0 {
		return DeliveryResult{
			Status: "hard_bounce",
			Error:  "No MX records found",
		}
	}

	// 3. Try each MX with IPv6/IPv4 preference
	var lastResult DeliveryResult

	// Determine delivery order: IPv6 first or IPv4 first
	tryIPv6First := d.ipRotator.PreferIPv6() && d.ipRotator.HasIPv6()
	tryIPv4 := d.ipRotator.HasIPv4()
	tryIPv6 := d.ipRotator.HasIPv6()

	for _, mx := range mxRecords {
		// Check context
		if ctx.Err() != nil {
			return DeliveryResult{Status: "timeout", Error: "Context canceled"}
		}

		// Try IPv6 first if preferred
		if tryIPv6First {
			result := d.tryDeliveryWithIPVersion(ctx, from, to, message, mx.Host, true, start)
			if result.Status == "delivered" || result.Status == "hard_bounce" {
				return result
			}
			lastResult = result

			// Fall back to IPv4 if IPv6 failed and IPv4 is available
			if tryIPv4 {
				result = d.tryDeliveryWithIPVersion(ctx, from, to, message, mx.Host, false, start)
				if result.Status == "delivered" || result.Status == "hard_bounce" {
					return result
				}
				lastResult = result
			}
		} else if tryIPv4 {
			// Try IPv4 first (or only)
			result := d.tryDeliveryWithIPVersion(ctx, from, to, message, mx.Host, false, start)
			if result.Status == "delivered" || result.Status == "hard_bounce" {
				return result
			}
			lastResult = result

			// Fall back to IPv6 if IPv4 failed and IPv6 is available
			if tryIPv6 {
				result = d.tryDeliveryWithIPVersion(ctx, from, to, message, mx.Host, true, start)
				if result.Status == "delivered" || result.Status == "hard_bounce" {
					return result
				}
				lastResult = result
			}
		} else {
			// No source IPs configured - use system default
			result := d.attemptDelivery(ctx, from, to, message, mx.Host, "", true)
			deliveryInfo := DeliveryInfo{From: from, To: to, MXHost: mx.Host}
			d.reputationTracker.RecordDeliveryAttempt("", result.Status == "delivered", nil, deliveryInfo)
			if result.Status == "delivered" || result.Status == "hard_bounce" {
				result.AttemptDurationMs = time.Since(start).Milliseconds()
				d.recordMetrics(result)
				return result
			}
			lastResult = result
		}
	}

	lastResult.AttemptDurationMs = time.Since(start).Milliseconds()
	d.recordMetrics(lastResult)
	return lastResult
}

// tryDeliveryWithIPVersion attempts delivery using either IPv6 or IPv4 source IPs and matching MX host IPs.
func (d *Deliverer) tryDeliveryWithIPVersion(ctx context.Context, from, to string, message []byte, mxHost string, useIPv6 bool, startTime time.Time) DeliveryResult {
	// Get source IPs for this version
	var sourceIPs []string
	if useIPv6 {
		sourceIPs = d.ipRotator.GetAllIPsV6()
	} else {
		sourceIPs = d.ipRotator.GetAllIPsV4()
	}

	// Filter by reputation
	healthyIPs := d.reputationTracker.GetHealthyIPs(sourceIPs)
	if len(healthyIPs) == 0 && len(sourceIPs) > 0 {
		d.logger.Warn("all source IPs degraded for IP version",
			"ipv6", useIPv6,
			"domain", to)
		// Use degraded IPs as last resort
		healthyIPs = sourceIPs
	}

	// Try each healthy source IP
	var lastResult DeliveryResult
	for _, sourceIP := range healthyIPs {
		// Check context
		if ctx.Err() != nil {
			return DeliveryResult{Status: "timeout", Error: "Context canceled"}
		}

		result := d.attemptDelivery(ctx, from, to, message, mxHost, sourceIP, useIPv6)
		lastResult = result

		// Track reputation
		deliveryInfo := DeliveryInfo{From: from, To: to, MXHost: mxHost}
		d.reputationTracker.RecordDeliveryAttempt(sourceIP, result.Status == "delivered", nil, deliveryInfo)

		if result.Status == "delivered" || result.Status == "hard_bounce" {
			return result
		}

		// If temp fail, try next source IP
		d.logger.Debug("delivery attempt failed",
			"mx", mxHost,
			"source_ip", sourceIP,
			"ipv6", useIPv6,
			"status", result.Status,
			"error", result.Error)
	}

	return lastResult
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

func (d *Deliverer) attemptDelivery(ctx context.Context, from, to string, msg []byte, mxHost, sourceIP string, preferIPv6 bool) DeliveryResult {
	// 1. Try to get a pooled connection
	var client *smtp.Client
	var reused bool
	if d.pool != nil {
		client = d.pool.Get(mxHost, sourceIP)
		if client != nil {
			reused = true
			d.logger.Debug("using pooled connection", "mx", mxHost)
		}
	}

	// 2. If no pooled connection, dial a new one
	if client == nil {
		var err error
		var result DeliveryResult
		client, result, err = d.dialAndHello(ctx, mxHost, sourceIP, preferIPv6)
		if err != nil {
			return result
		}
	}

	// 3. Deliver message logic
	// Note: We need to handle Close() manually if we want to reuse the client
	// So we don't defer client.Close() here if we intend to pool it.
	// But if dialAndHello returned a client, we own it.

	return d.performDeliveryTransaction(client, from, to, msg, mxHost, sourceIP, reused)
}

func (d *Deliverer) performDeliveryTransaction(client *smtp.Client, from, to string, msg []byte, mxHost, sourceIP string, reused bool) DeliveryResult {
	// Safety cleanup in case of panic or non-pooled exit
	// We wrap this logic deeply. To properly manage pooling, we handle close/reset manually.
	// But defer Close() is safe because calling Close() on closed client is fine,
	// and if we reuse, we won't Close() it.
	// Wait, if we put back in pool, we must NOT Close().
	// So we can't defer Close() blindly.

	err := d.deliverPayload(client, from, to, msg)
	if err != nil {
		// If error occurred on a reused connection, it might be stale.
		// We could retry? For now, we fail and let client retry.
		client.Close()

		// If reused and error is network/EOF, we might want to suggest retry.
		return d.mapSMTPError(err, mxHost, sourceIP)
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
		Status:      "delivered",
		SMTPCode:    250,
		SMTPMessage: "OK",
		MXHost:      mxHost,
		SourceIP:    sourceIP,
	}
}

func (d *Deliverer) deliverPayload(client *smtp.Client, from, to string, msg []byte) error {
	// MAIL FROM
	mailFrom := from
	if d.srs != nil {
		if rewritten, err := d.srs.Forward(from); err == nil {
			mailFrom = rewritten
		}
	}
	if err := client.Mail(mailFrom, nil); err != nil {
		return err
	}

	// RCPT TO
	if err := client.Rcpt(to, nil); err != nil {
		return err
	}

	// DATA
	w, err := client.Data()
	if err != nil {
		return err
	}
	if w == nil {
		return fmt.Errorf("DATA command returned nil writer")
	}

	if _, err := w.Write(msg); err != nil {
		return err
	}

	if err := w.Close(); err != nil {
		return err
	}

	return nil
}

func (d *Deliverer) dialAndHello(ctx context.Context, mxHost, sourceIP string, preferIPv6 bool) (*smtp.Client, DeliveryResult, error) {
	// Resolve MX host to IP addresses
	mxIPs, err := d.mxLookup.dnsResolver.LookupHost(ctx, mxHost)
	if err != nil {
		return nil, DeliveryResult{
			Status:   "temp_fail",
			MXHost:   mxHost,
			SourceIP: sourceIP,
			Error:    fmt.Sprintf("Failed to resolve MX host: %v", err),
		}, err
	}

	// Filter MX IPs by version (IPv6 or IPv4)
	var targetIP string
	for _, ip := range mxIPs {
		parsedIP := net.ParseIP(ip)
		if parsedIP == nil {
			continue
		}

		isV4 := parsedIP.To4() != nil
		if preferIPv6 && !isV4 {
			targetIP = ip
			break
		} else if !preferIPv6 && isV4 {
			targetIP = ip
			break
		}
	}

	// Fallback if no matching IP version found
	if targetIP == "" {
		if len(mxIPs) > 0 {
			targetIP = mxIPs[0]
			d.logger.Debug("no matching IP version for MX, using first available",
				"mx", mxHost,
				"prefer_ipv6", preferIPv6,
				"target_ip", targetIP)
		} else {
			return nil, DeliveryResult{
				Status:   "temp_fail",
				MXHost:   mxHost,
				SourceIP: sourceIP,
				Error:    "No IP addresses resolved for MX host",
			}, fmt.Errorf("no IP")
		}
	}

	// TCP connection to port 25 (SMTP)
	dialer := &net.Dialer{
		Timeout: time.Duration(d.config.ConnectionTimeoutSeconds) * time.Second,
	}
	if sourceIP != "" {
		ip := net.ParseIP(sourceIP)
		if ip == nil {
			return nil, DeliveryResult{
				Status:   "error",
				MXHost:   mxHost,
				SourceIP: sourceIP,
				Error:    fmt.Sprintf("Invalid source IP address: %s", sourceIP),
			}, fmt.Errorf("invalid IP")
		}
		dialer.LocalAddr = &net.TCPAddr{IP: ip}
	}

	// Connect to the target IP
	conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(targetIP, "25"))
	if err != nil {
		// Check if error is due to source IP binding failure
		if sourceIP != "" && isBindError(err) {
			d.logger.Error("failed to bind to source IP",
				"source_ip", sourceIP,
				"error", err)
			return nil, DeliveryResult{
				Status:   "error",
				MXHost:   mxHost,
				SourceIP: sourceIP,
				Error:    fmt.Sprintf("Cannot bind to source IP %s: %v", sourceIP, err),
			}, err
		}
		return nil, DeliveryResult{Status: "temp_fail", MXHost: mxHost, SourceIP: sourceIP, Error: err.Error()}, err
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

	// Create initial SMTP client (plaintext)
	client := smtp.NewClient(conn)
	// Do not defer client.Close() here, we return it.

	// Send EHLO
	if err := client.Hello("localhost"); err != nil {
		client.Close()
		return nil, DeliveryResult{Status: "temp_fail", MXHost: mxHost, SourceIP: sourceIP, Error: err.Error()}, err
	}

	// STARTTLS
	if ok, _ := client.Extension("STARTTLS"); ok {
		d.logger.Debug("STARTTLS supported, upgrading connection", "mx", mxHost)

		tlsConfig := &tls.Config{
			ServerName: host,
			MinVersion: tls.VersionTLS12,
		}

		// Manual STARTTLS upgrade logic
		// We avoid closing 'client' here if it closes the underlying connection.
		// Instead, we just stop using it and write directly to 'conn'.
		// Since we just did Hello(), buffer should be empty.

		if _, err := conn.Write([]byte("STARTTLS\r\n")); err != nil {
			client.Close()
			return nil, DeliveryResult{Status: "temp_fail", MXHost: mxHost, SourceIP: sourceIP, Error: fmt.Sprintf("STARTTLS send failed: %v", err)}, err
		}

		// Read 220 response
		buf := make([]byte, 1024)
		n, err := conn.Read(buf)
		if err != nil {
			client.Close() // Best effort
			return nil, DeliveryResult{Status: "temp_fail", MXHost: mxHost, SourceIP: sourceIP, Error: fmt.Sprintf("STARTTLS response failed: %v", err)}, err
		}

		response := string(buf[:n])
		if len(response) < 3 || response[:3] != "220" {
			client.Close()
			return nil, DeliveryResult{Status: "temp_fail", MXHost: mxHost, SourceIP: sourceIP, Error: fmt.Sprintf("STARTTLS rejected: %s", response)}, fmt.Errorf("bad response")
		}

		// Wrap connection in TLS
		tlsConn := tls.Client(conn, tlsConfig)

		// Perform TLS handshake with context timeout and panic recovery
		handshakeDone := make(chan error, 1)
		recovery.SafeGo(d.logger, "tls-handshake", func() {
			handshakeDone <- tlsConn.Handshake()
		})

		select {
		case err := <-handshakeDone:
			if err != nil {
				// Don't close tlsConn yet if we want to return specific error?
				// But we are returning nil client.
				client.Close()
				return nil, DeliveryResult{Status: "temp_fail", MXHost: mxHost, SourceIP: sourceIP, Error: fmt.Sprintf("TLS handshake failed: %v", err)}, err
			}
		case <-ctx.Done():
			client.Close()
			return nil, DeliveryResult{Status: "timeout", MXHost: mxHost, SourceIP: sourceIP, Error: "TLS handshake timed out"}, ctx.Err()
		}

		// Create new SMTP client using the TLS connection
		// Note: The old 'client' is abandoned. We invoke quit/close on it later?
		// No, we can't because it would close the underlying conn which we just upgraded.
		// We rely on GC to clean up the struct, and we manage the 'tlsConn' now.
		client = smtp.NewClient(tlsConn)

		// Send EHLO again
		if err := client.Hello("localhost"); err != nil {
			client.Close()
			return nil, DeliveryResult{Status: "temp_fail", MXHost: mxHost, SourceIP: sourceIP, Error: err.Error()}, err
		}

		d.logger.Debug("STARTTLS upgrade successful", "mx", mxHost)
	} else {
		d.logger.Warn("STARTTLS not supported", "mx", mxHost)
	}

	success = true // Prevent deferred close
	return client, DeliveryResult{}, nil
}

func (d *Deliverer) mapSMTPError(err error, mxHost, sourceIP string) DeliveryResult {
	res := DeliveryResult{MXHost: mxHost, SourceIP: sourceIP}
	if smtpErr, ok := err.(*smtp.SMTPError); ok {
		res.SMTPCode = smtpErr.Code
		res.SMTPMessage = smtpErr.Message
		if smtpErr.Code >= 500 {
			res.Status = "hard_bounce"
		} else {
			res.Status = "temp_fail"
		}
	} else {
		res.Status = "error"
		res.Error = err.Error()
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
