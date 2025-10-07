/*
Package delivery handles direct SMTP delivery to recipient MX servers.

The delivery engine establishes direct connections to recipient mail servers,
performs SMTP transactions, and applies intelligent retry logic with exponential
backoff. It supports IPv6-first delivery, source IP rotation, DKIM signing,
and destination throttling.

Key Components:
  - Engine: Main delivery orchestration with context-aware SMTP sessions
  - MXLookup: DNS MX record resolution with caching
  - DNSResolver: Custom DNS resolver with round-robin and UDP→TCP fallback
  - IPRotator: Source IP selection (round-robin, random, hash-domain strategies)
  - IPReputation: Tracks and removes degraded IPs from rotation
  - RetryScheduler: Exponential backoff with greylist fast-retry
  - ErrorClassifier: Categorizes SMTP errors (temporary, permanent, greylist, network)
  - CircuitBreaker: Prevents accepting messages during delivery failures
  - DestinationThrottle: Per-domain rate limiting to prevent spam-like behavior

Delivery Flow:
 1. Resolve MX records (with caching)
 2. Select source IP based on configured strategy
 3. Attempt IPv6 connection, fallback to IPv4
 4. Establish SMTP session with STARTTLS
 5. Send message (with optional DKIM signing)
 6. Classify result and schedule retry if needed
 7. Update IP reputation on permanent failures
*/
package delivery

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"hash/fnv"
	"io"
	"math/rand"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"fune/internal/config"
	"fune/internal/dkim"
	"fune/internal/queue"

	"github.com/emersion/go-smtp"
	"go.uber.org/zap"
)

// DestinationThrottle tracks last delivery attempt per domain to prevent spam-like behavior.
// It enforces a minimum interval between consecutive delivery attempts to the same domain,
// preventing the delivery engine from appearing as a spam source to recipient servers.
type DestinationThrottle struct {
	mu           sync.RWMutex
	lastAttempts map[string]time.Time
	minInterval  time.Duration
}

// NewDestinationThrottle creates a new destination throttle with the specified minimum interval.
// The minIntervalSeconds parameter defines the minimum seconds between delivery attempts
// to the same recipient domain.
func NewDestinationThrottle(minIntervalSeconds int) *DestinationThrottle {
	return &DestinationThrottle{
		lastAttempts: make(map[string]time.Time),
		minInterval:  time.Duration(minIntervalSeconds) * time.Second,
	}
}

// ShouldThrottle checks if we should throttle delivery to this domain
// Returns true if last attempt was too recent, along with time until next allowed attempt
func (dt *DestinationThrottle) ShouldThrottle(domain string) (bool, time.Duration) {
	dt.mu.RLock()
	lastAttempt, exists := dt.lastAttempts[domain]
	dt.mu.RUnlock()

	if !exists {
		return false, 0
	}

	elapsed := time.Since(lastAttempt)
	if elapsed < dt.minInterval {
		waitTime := dt.minInterval - elapsed
		return true, waitTime
	}

	return false, 0
}

// RecordAttempt records a delivery attempt to a domain
func (dt *DestinationThrottle) RecordAttempt(domain string) {
	dt.mu.Lock()
	dt.lastAttempts[domain] = time.Now()
	dt.mu.Unlock()
}

// Cleanup removes old entries to prevent unbounded memory growth
func (dt *DestinationThrottle) Cleanup(maxAge time.Duration) {
	dt.mu.Lock()
	defer dt.mu.Unlock()

	cutoff := time.Now().Add(-maxAge)
	for domain, lastAttempt := range dt.lastAttempts {
		if lastAttempt.Before(cutoff) {
			delete(dt.lastAttempts, domain)
		}
	}
}

// DeliveryMetrics defines the interface for recording delivery metrics to Prometheus or other
// monitoring systems. It tracks delivery outcomes, durations, and circuit breaker state changes.
type DeliveryMetrics interface {
	RecordDeliveryAttempt(outcome string, duration float64)
	SetCircuitBreakerState(state int)
	RecordCircuitBreakerTransition(fromState, toState string)
}

// Deliverer is the main delivery engine that handles direct SMTP delivery to recipient MX servers.
// It coordinates MX lookups, source IP rotation, SMTP session management, delivery attempts,
// error classification, retry scheduling, and IP reputation tracking. The engine attempts IPv6
// first before falling back to IPv4, and supports DKIM signing and STARTTLS encryption.
type Deliverer struct {
	mu                sync.RWMutex
	config            *config.OutboundConfig
	mxLookup          *MXLookup
	logger            *zap.Logger
	ipRotator         *IPRotator
	throttle          *DestinationThrottle
	circuitBreaker    *CircuitBreaker
	metrics           DeliveryMetrics
	reputationTracker *IPReputationTracker
}

// DeliveryResult contains the complete result of a delivery attempt, including success status,
// SMTP response codes, the MX host used, the source IP used, any errors encountered, and
// the duration of the attempt in milliseconds.
type DeliveryResult struct {
	Success      bool
	SMTPCode     int
	SMTPResponse string
	MXHost       string
	SourceIP     string
	Error        *DeliveryError
	DurationMs   int64
}

// NewDeliverer creates a new delivery engine with the specified configuration.
// It initializes the circuit breaker (if enabled), IP reputation tracker, IP rotator,
// and destination throttle. The circuit breaker can be disabled via configuration,
// which means the service will continue accepting requests even during delivery failures.
func NewDeliverer(config *config.OutboundConfig, mxLookup *MXLookup, logger *zap.Logger, reputationConfig *config.ReputationConfig) *Deliverer {
	// Create circuit breaker with configured values
	var circuitBreaker *CircuitBreaker
	if !config.CircuitBreakerEnabled {
		logger.Warn("circuit breaker is DISABLED - service will accept requests even during delivery failures")
		circuitBreaker = nil // Disabled
	} else {
		openTimeout := time.Duration(config.CircuitBreakerOpenTimeoutSecs) * time.Second
		circuitBreaker = NewCircuitBreaker(
			config.CircuitBreakerFailureThreshold,
			config.CircuitBreakerSuccessThreshold,
			openTimeout,
			logger,
		)
		logger.Info("circuit breaker enabled",
			zap.Int("failure_threshold", config.CircuitBreakerFailureThreshold),
			zap.Int("success_threshold", config.CircuitBreakerSuccessThreshold),
			zap.Duration("open_timeout", openTimeout))
	}

	// Create reputation tracker
	reputationTracker := NewIPReputationTracker(reputationConfig, logger)

	return &Deliverer{
		config:            config,
		mxLookup:          mxLookup,
		logger:            logger,
		ipRotator:         NewIPRotator(config.SourceIPs, config.SourceIPSelection),
		throttle:          NewDestinationThrottle(config.PerDomainIntervalSeconds),
		circuitBreaker:    circuitBreaker,
		reputationTracker: reputationTracker,
	}
}

// DeliverMessage attempts to deliver a message directly to the recipient's MX servers.
// It performs domain throttling, MX lookups, filters out degraded IPs, and tries each MX
// server in priority order with source IP rotation on local failures. The method returns
// a DeliveryResult containing the outcome, SMTP codes, and any errors encountered.
// On success, the circuit breaker is notified. On failure, appropriate retry scheduling
// and IP reputation updates occur.
func (d *Deliverer) DeliverMessage(ctx context.Context, msg *queue.QueuedMessage) *DeliveryResult {
	startTime := time.Now()

	d.logger.Info("starting delivery attempt",
		zap.String("message_id", msg.MessageID),
		zap.String("to", msg.ToAddr),
		zap.String("to_domain", msg.ToDomain),
		zap.Int("attempt", msg.Attempts+1))

	// Check if we should throttle delivery to this domain
	if shouldThrottle, waitTime := d.throttle.ShouldThrottle(msg.ToDomain); shouldThrottle {
		d.logger.Info("throttling delivery to domain",
			zap.String("message_id", msg.MessageID),
			zap.String("domain", msg.ToDomain),
			zap.Duration("wait_time", waitTime),
			zap.Int("min_interval_seconds", d.config.PerDomainIntervalSeconds))

		result := &DeliveryResult{
			Success: false,
			Error: &DeliveryError{
				Category: ErrorThrottled,
				Message:  fmt.Sprintf("rate limiting active for domain %s, retry in %v", msg.ToDomain, waitTime.Round(time.Second)),
			},
			DurationMs: time.Since(startTime).Milliseconds(),
		}
		d.recordDeliveryMetrics(result)
		return result
	}

	// Record this delivery attempt
	d.throttle.RecordAttempt(msg.ToDomain)

	// Lookup MX records
	mxRecords, err := d.mxLookup.Lookup(ctx, msg.ToDomain)
	if err != nil {
		d.logger.Error("MX lookup failed",
			zap.String("message_id", msg.MessageID),
			zap.String("domain", msg.ToDomain),
			zap.Error(err))

		result := &DeliveryResult{
			Success:    false,
			Error:      ClassifyError(0, "", err),
			DurationMs: time.Since(startTime).Milliseconds(),
		}
		d.recordDeliveryMetrics(result)
		return result
	}

	d.logger.Debug("MX records found",
		zap.String("domain", msg.ToDomain),
		zap.Int("mx_count", len(mxRecords)))

	// Try each MX in priority order with source IP rotation on local failures
	var lastResult *DeliveryResult
	allSourceIPs := d.ipRotator.GetAllIPs() // Get all available source IPs

	// Filter out degraded IPs
	sourceIPs := d.reputationTracker.GetHealthyIPs(allSourceIPs)

	if len(sourceIPs) == 0 {
		if len(allSourceIPs) > 0 {
			// All IPs are degraded, log warning
			d.logger.Warn("all source IPs are degraded, using default",
				zap.String("message_id", msg.MessageID),
				zap.Int("total_ips", len(allSourceIPs)))
		}
		sourceIPs = []string{""} // Empty string means use default source IP
	} else if len(sourceIPs) < len(allSourceIPs) {
		d.logger.Info("some source IPs are degraded",
			zap.String("message_id", msg.MessageID),
			zap.Int("healthy_ips", len(sourceIPs)),
			zap.Int("total_ips", len(allSourceIPs)))
	}

	for i, mx := range mxRecords {
		// Try delivery with source IP rotation on local failures
		for ipIdx, sourceIP := range sourceIPs {
			d.logger.Debug("attempting MX server",
				zap.String("message_id", msg.MessageID),
				zap.String("mx_host", mx.Host),
				zap.Int("priority", int(mx.Priority)),
				zap.Int("mx_index", i+1),
				zap.Int("total_mx", len(mxRecords)),
				zap.String("source_ip", sourceIP),
				zap.Int("source_ip_index", ipIdx+1),
				zap.Int("total_source_ips", len(sourceIPs)))

			result := d.attemptDelivery(ctx, msg, mx.Host, sourceIP)
			result.DurationMs = time.Since(startTime).Milliseconds()
			lastResult = result

			// Track reputation for this IP
			deliveryInfo := DeliveryInfo{
				From:           msg.FromAddr,
				To:             msg.ToAddr,
				Subject:        msg.Subject,
				IdempotencyKey: msg.IdempotencyKey,
				MXHost:         mx.Host,
			}
			d.reputationTracker.RecordDeliveryAttempt(sourceIP, result.Success, result.Error, deliveryInfo)

			// Record circuit breaker metrics (if enabled)
			if result.Success {
				if d.circuitBreaker != nil {
					d.circuitBreaker.RecordSuccess()
				}

				d.logger.Info("delivery successful",
					zap.String("message_id", msg.MessageID),
					zap.String("mx_host", mx.Host),
					zap.String("source_ip", sourceIP),
					zap.Int64("duration_ms", result.DurationMs))
				d.recordDeliveryMetrics(result)
				return result
			}

			// Record failure (if circuit breaker enabled)
			isLocalError := IsLocalError(result.Error)
			if d.circuitBreaker != nil {
				d.circuitBreaker.RecordFailure(isLocalError)
			}

			d.logger.Warn("MX delivery failed",
				zap.String("message_id", msg.MessageID),
				zap.String("mx_host", mx.Host),
				zap.String("source_ip", sourceIP),
				zap.Int("smtp_code", result.SMTPCode),
				zap.String("error", result.Error.Message),
				zap.Bool("is_local_error", isLocalError))

			// If permanent error, don't try other IPs or MX servers
			if result.Error != nil && result.Error.Category == ErrorPermanent {
				d.logger.Info("permanent error, not trying other MX servers",
					zap.String("message_id", msg.MessageID),
					zap.String("error_category", string(result.Error.Category)))
				d.recordDeliveryMetrics(result)
				return result
			}

			// If local network error or reputation error, try next source IP
			if (isLocalError || result.Error.Category == ErrorReputation) && ipIdx < len(sourceIPs)-1 {
				d.logger.Info("local network or reputation error, trying next source IP",
					zap.String("message_id", msg.MessageID),
					zap.String("failed_source_ip", sourceIP),
					zap.String("next_source_ip", sourceIPs[ipIdx+1]),
					zap.String("error_category", string(result.Error.Category)))
				continue
			}

			// Non-local error or last source IP, try next MX
			break
		}

		// If we got a permanent error, already returned above
		// If last MX, will return below
	}

	// All MX servers failed
	d.logger.Error("all MX servers failed",
		zap.String("message_id", msg.MessageID),
		zap.String("domain", msg.ToDomain),
		zap.Int("mx_count", len(mxRecords)))

	// Return last result if available
	if lastResult != nil {
		d.recordDeliveryMetrics(lastResult)
		return lastResult
	}

	result := &DeliveryResult{
		Success: false,
		Error: &DeliveryError{
			Category: ErrorTemporary,
			Message:  "All MX servers failed",
		},
		DurationMs: time.Since(startTime).Milliseconds(),
	}
	d.recordDeliveryMetrics(result)
	return result
}

// attemptDelivery tries to deliver to a specific MX server
// Properly handles multihomed MX servers by trying all resolved IPs
func (d *Deliverer) attemptDelivery(ctx context.Context, msg *queue.QueuedMessage, mxHost string, sourceIP string) *DeliveryResult {
	// Resolve MX hostname to all IP addresses
	resolver := net.DefaultResolver
	addrs, err := resolver.LookupHost(ctx, mxHost)
	if err != nil {
		d.logger.Error("failed to resolve MX hostname",
			zap.String("message_id", msg.MessageID),
			zap.String("mx_host", mxHost),
			zap.Error(err))
		return &DeliveryResult{
			Success: false,
			MXHost:  mxHost,
			Error:   ClassifyError(0, "", err),
		}
	}

	d.logger.Debug("resolved MX hostname",
		zap.String("mx_host", mxHost),
		zap.Strings("addresses", addrs),
		zap.Int("ip_count", len(addrs)))

	// Separate IPv6 and IPv4 addresses
	var ipv6Addrs, ipv4Addrs []string
	for _, addr := range addrs {
		ip := net.ParseIP(addr)
		if ip != nil {
			if ip.To4() == nil {
				// IPv6 address
				ipv6Addrs = append(ipv6Addrs, addr)
			} else {
				// IPv4 address
				ipv4Addrs = append(ipv4Addrs, addr)
			}
		}
	}

	// Limit total number of IPs to try (protect against malicious multihomed hosts)
	maxIPs := d.config.MaxIPsPerMX
	totalAttempts := 0

	// Try IPv6 addresses first (up to limit)
	for _, ipAddr := range ipv6Addrs {
		if totalAttempts >= maxIPs {
			d.logger.Warn("reached max IP limit for MX host",
				zap.String("mx_host", mxHost),
				zap.Int("max_ips", maxIPs),
				zap.Int("skipped_ipv6", len(ipv6Addrs)-totalAttempts))
			break
		}

		d.logger.Debug("attempting IPv6 address",
			zap.String("message_id", msg.MessageID),
			zap.String("mx_host", mxHost),
			zap.String("ip_address", ipAddr),
			zap.Int("attempt", totalAttempts+1),
			zap.Int("max_attempts", maxIPs))

		result := d.tryDeliveryToIP(ctx, msg, mxHost, ipAddr, sourceIP, "tcp6")
		totalAttempts++

		if result.Success || (result.Error != nil && result.Error.Category == ErrorPermanent) {
			return result
		}
	}

	// Try IPv4 addresses (up to remaining limit)
	for _, ipAddr := range ipv4Addrs {
		if totalAttempts >= maxIPs {
			d.logger.Warn("reached max IP limit for MX host",
				zap.String("mx_host", mxHost),
				zap.Int("max_ips", maxIPs),
				zap.Int("total_ips", len(ipv6Addrs)+len(ipv4Addrs)),
				zap.Int("tried", totalAttempts))
			break
		}

		d.logger.Debug("attempting IPv4 address",
			zap.String("message_id", msg.MessageID),
			zap.String("mx_host", mxHost),
			zap.String("ip_address", ipAddr),
			zap.Int("attempt", totalAttempts+1),
			zap.Int("max_attempts", maxIPs))

		result := d.tryDeliveryToIP(ctx, msg, mxHost, ipAddr, sourceIP, "tcp4")
		totalAttempts++

		if result.Success || (result.Error != nil && result.Error.Category == ErrorPermanent) {
			return result
		}
	}

	// All IPs failed
	return &DeliveryResult{
		Success: false,
		MXHost:  mxHost,
		Error: &DeliveryError{
			Category: ErrorTemporary,
			Message:  fmt.Sprintf("all IP addresses failed for %s (%d IPv6, %d IPv4)", mxHost, len(ipv6Addrs), len(ipv4Addrs)),
		},
	}
}

// tryDeliveryToIP attempts delivery to a specific IP address
func (d *Deliverer) tryDeliveryToIP(ctx context.Context, msg *queue.QueuedMessage, mxHost string, targetIP string, sourceIP string, network string) *DeliveryResult {
	// Create dialer with source IP binding
	dialer := &net.Dialer{
		Timeout: time.Duration(d.config.ConnectionTimeoutSeconds) * time.Second,
	}

	// Bind to source IP if specified
	if sourceIP != "" {
		ip := net.ParseIP(sourceIP)
		if ip != nil {
			// Determine if source IP is IPv4 or IPv6
			if ip.To4() != nil {
				// IPv4 source
				if network == "tcp6" {
					// Skip IPv6 attempt with IPv4 source
					return &DeliveryResult{
						Success:  false,
						MXHost:   mxHost,
						SourceIP: sourceIP,
						Error: &DeliveryError{
							Category: ErrorTemporary,
							Message:  "IPv4 source IP cannot be used for IPv6 connection",
						},
					}
				}
				dialer.LocalAddr = &net.TCPAddr{IP: ip}
			} else {
				// IPv6 source
				if network == "tcp4" {
					// Skip IPv4 attempt with IPv6 source
					return &DeliveryResult{
						Success:  false,
						MXHost:   mxHost,
						SourceIP: sourceIP,
						Error: &DeliveryError{
							Category: ErrorTemporary,
							Message:  "IPv6 source IP cannot be used for IPv4 connection",
						},
					}
				}
				dialer.LocalAddr = &net.TCPAddr{IP: ip}
			}
		}
	}

	// Try port 25 (standard SMTP) - connect to specific IP
	addr := net.JoinHostPort(targetIP, "25")

	// Connect with context awareness and specified network (tcp4/tcp6)
	conn, err := dialer.DialContext(ctx, network, addr)
	if err != nil {
		return &DeliveryResult{
			Success:  false,
			MXHost:   mxHost,
			SourceIP: sourceIP,
			Error:    ClassifyError(0, "", err),
		}
	}
	defer conn.Close()

	// Set overall SMTP timeout
	conn.SetDeadline(time.Now().Add(time.Duration(d.config.SMTPTimeoutSeconds) * time.Second))

	// Create SMTP client with STARTTLS
	tlsConfig := &tls.Config{
		ServerName:         mxHost,
		InsecureSkipVerify: false,
	}

	client, err := smtp.NewClientStartTLS(conn, tlsConfig)
	if err != nil {
		// STARTTLS failed, try without TLS
		d.logger.Warn("STARTTLS failed, trying without TLS",
			zap.String("mx_host", mxHost),
			zap.Error(err))

		client = smtp.NewClient(conn)
	} else {
		d.logger.Debug("STARTTLS successful",
			zap.String("mx_host", mxHost))
	}
	defer client.Close()

	// MAIL FROM
	if err := client.Mail(msg.FromAddr, nil); err != nil {
		smtpCode, smtpResp := extractSMTPError(err)
		return &DeliveryResult{
			Success:      false,
			MXHost:       mxHost,
			SourceIP:     sourceIP,
			SMTPCode:     smtpCode,
			SMTPResponse: smtpResp,
			Error:        ClassifyError(smtpCode, smtpResp, err),
		}
	}

	// RCPT TO
	if err := client.Rcpt(msg.ToAddr, nil); err != nil {
		smtpCode, smtpResp := extractSMTPError(err)
		return &DeliveryResult{
			Success:      false,
			MXHost:       mxHost,
			SourceIP:     sourceIP,
			SMTPCode:     smtpCode,
			SMTPResponse: smtpResp,
			Error:        ClassifyError(smtpCode, smtpResp, err),
		}
	}

	// DATA
	dataWriter, err := client.Data()
	if err != nil {
		smtpCode, smtpResp := extractSMTPError(err)
		return &DeliveryResult{
			Success:      false,
			MXHost:       mxHost,
			SourceIP:     sourceIP,
			SMTPCode:     smtpCode,
			SMTPResponse: smtpResp,
			Error:        ClassifyError(smtpCode, smtpResp, err),
		}
	}

	// Prepare message - sign with DKIM if credentials provided
	messageToSend := msg.RawMessage
	if msg.DKIMPrivateKey != "" {
		d.logger.Debug("signing message with DKIM",
			zap.String("message_id", msg.MessageID),
			zap.String("dkim_selector", msg.DKIMSelector),
			zap.String("dkim_domain", msg.DKIMDomain))

		signedMessage, err := dkim.SignMessage(msg.RawMessage, msg.DKIMPrivateKey, msg.DKIMSelector, msg.DKIMDomain)
		if err != nil {
			dataWriter.Close()
			d.logger.Error("DKIM signing failed",
				zap.String("message_id", msg.MessageID),
				zap.Error(err))
			return &DeliveryResult{
				Success:  false,
				MXHost:   mxHost,
				SourceIP: sourceIP,
				Error: &DeliveryError{
					Category: ErrorPermanent,
					Message:  fmt.Sprintf("DKIM signing failed: %v", err),
				},
			}
		}
		messageToSend = signedMessage
		d.logger.Debug("DKIM signature added successfully",
			zap.String("message_id", msg.MessageID))
	}

	// Write message data
	if _, err := io.Copy(dataWriter, bytes.NewReader(messageToSend)); err != nil {
		dataWriter.Close()
		return &DeliveryResult{
			Success:  false,
			MXHost:   mxHost,
			SourceIP: sourceIP,
			Error:    ClassifyError(0, "", err),
		}
	}

	// Close data writer (sends final .)
	if err := dataWriter.Close(); err != nil {
		smtpCode, smtpResp := extractSMTPError(err)
		return &DeliveryResult{
			Success:      false,
			MXHost:       mxHost,
			SourceIP:     sourceIP,
			SMTPCode:     smtpCode,
			SMTPResponse: smtpResp,
			Error:        ClassifyError(smtpCode, smtpResp, err),
		}
	}

	// QUIT
	client.Quit()

	// Success!
	return &DeliveryResult{
		Success:      true,
		SMTPCode:     250,
		SMTPResponse: "OK",
		MXHost:       mxHost,
		SourceIP:     sourceIP,
	}
}

// extractSMTPError extracts SMTP code and response from error
func extractSMTPError(err error) (int, string) {
	if smtpErr, ok := err.(*smtp.SMTPError); ok {
		return smtpErr.Code, smtpErr.Message
	}
	return 0, err.Error()
}

// IPRotator handles source IP selection using configurable strategies.
// It supports three selection strategies: round-robin (balanced distribution),
// random (unpredictable selection), and hash-domain (consistent IP per domain).
// The rotator is thread-safe and uses atomic operations for the round-robin counter.
type IPRotator struct {
	ips      []string
	strategy string
	counter  uint32 // atomic counter for round-robin
	random   *rand.Rand
}

// NewIPRotator creates a new IP rotator with the specified IPs and selection strategy.
// Valid strategies are "round-robin", "random", and "hash-domain". If an invalid strategy
// is provided, round-robin is used as the default.
func NewIPRotator(ips []string, strategy string) *IPRotator {
	return &IPRotator{
		ips:      ips,
		strategy: strategy,
		counter:  0,
		random:   rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

// SelectIP chooses a source IP based on the configured strategy.
// For round-robin, it cycles through IPs in order using an atomic counter.
// For random, it selects a random IP from the pool.
// For hash-domain, it uses FNV-1a hashing to consistently map domains to IPs.
// Returns an empty string if no IPs are configured, indicating no source binding.
func (r *IPRotator) SelectIP(domain string) string {
	if len(r.ips) == 0 {
		return "" // No source IP binding
	}

	if len(r.ips) == 1 {
		return r.ips[0]
	}

	switch r.strategy {
	case "round-robin":
		// Use atomic operations for thread-safe counter
		count := atomic.AddUint32(&r.counter, 1) - 1
		ip := r.ips[int(count)%len(r.ips)]
		return ip

	case "random":
		return r.ips[r.random.Intn(len(r.ips))]

	case "hash-domain":
		// Consistent hashing based on domain
		h := fnv.New32a()
		h.Write([]byte(domain))
		idx := int(h.Sum32()) % len(r.ips)
		return r.ips[idx]

	default:
		// Default to round-robin
		count := atomic.AddUint32(&r.counter, 1) - 1
		ip := r.ips[int(count)%len(r.ips)]
		return ip
	}
}

// GetAllIPs returns all configured source IPs for failover scenarios.
// It returns a copy of the IP list to prevent external modification.
// Returns nil if no IPs are configured.
func (r *IPRotator) GetAllIPs() []string {
	if len(r.ips) == 0 {
		return nil
	}
	// Return copy to prevent modification
	ips := make([]string, len(r.ips))
	copy(ips, r.ips)
	return ips
}

// GetCircuitBreaker returns the circuit breaker instance for health checking and monitoring.
// Returns nil if the circuit breaker is disabled via configuration.
func (d *Deliverer) GetCircuitBreaker() *CircuitBreaker {
	return d.circuitBreaker
}

// GetReputationTracker returns the IP reputation tracker instance for monitoring
// degraded IPs and reputation events.
func (d *Deliverer) GetReputationTracker() *IPReputationTracker {
	return d.reputationTracker
}

// SetMetrics sets the metrics recorder for delivery, enabling Prometheus metrics
// for delivery attempts, circuit breaker state, and other delivery statistics.
func (d *Deliverer) SetMetrics(metrics DeliveryMetrics) {
	d.metrics = metrics
}

// ReloadConfig updates the delivery configuration during a hot reload (triggered by SIGHUP).
// It updates source IPs, IP selection strategy, throttle settings, and circuit breaker thresholds.
// The circuit breaker can be enabled or disabled dynamically. This method is thread-safe.
func (d *Deliverer) ReloadConfig(newConfig *config.OutboundConfig) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	d.logger.Info("reloading delivery configuration",
		zap.Int("old_source_ips", len(d.config.SourceIPs)),
		zap.Int("new_source_ips", len(newConfig.SourceIPs)),
		zap.String("old_ip_selection", d.config.SourceIPSelection),
		zap.String("new_ip_selection", newConfig.SourceIPSelection))

	// Update config
	d.config = newConfig

	// Recreate IP rotator with new IPs and selection strategy
	d.ipRotator = NewIPRotator(newConfig.SourceIPs, newConfig.SourceIPSelection)

	// Update throttle settings
	d.throttle = NewDestinationThrottle(newConfig.PerDomainIntervalSeconds)

	// Update circuit breaker settings if enabled
	if newConfig.CircuitBreakerEnabled {
		if d.circuitBreaker == nil {
			// Circuit breaker was disabled, now enabled - create it
			openTimeout := time.Duration(newConfig.CircuitBreakerOpenTimeoutSecs) * time.Second
			d.circuitBreaker = NewCircuitBreaker(
				newConfig.CircuitBreakerFailureThreshold,
				newConfig.CircuitBreakerSuccessThreshold,
				openTimeout,
				d.logger,
			)
			if d.metrics != nil {
				d.circuitBreaker.SetMetrics(d.metrics)
			}
			d.logger.Info("circuit breaker enabled via config reload")
		} else {
			// Circuit breaker exists, update thresholds
			openTimeout := time.Duration(newConfig.CircuitBreakerOpenTimeoutSecs) * time.Second
			d.circuitBreaker.mu.Lock()
			d.circuitBreaker.failureThreshold = newConfig.CircuitBreakerFailureThreshold
			d.circuitBreaker.successThreshold = newConfig.CircuitBreakerSuccessThreshold
			d.circuitBreaker.openTimeout = openTimeout
			d.circuitBreaker.mu.Unlock()
			d.logger.Info("circuit breaker settings updated")
		}
	} else if d.circuitBreaker != nil {
		// Circuit breaker was enabled, now disabled
		d.circuitBreaker = nil
		d.logger.Warn("circuit breaker DISABLED via config reload")
	}

	d.logger.Info("delivery configuration reloaded successfully")
	return nil
}

// recordDeliveryMetrics records metrics for a delivery result
func (d *Deliverer) recordDeliveryMetrics(result *DeliveryResult) {
	if d.metrics == nil {
		return
	}

	var outcome string
	if result.Success {
		outcome = "success"
	} else if result.Error != nil {
		switch result.Error.Category {
		case ErrorPermanent:
			outcome = "permanent_error"
		case ErrorTemporary, ErrorGreylist:
			outcome = "temporary_error"
		case ErrorNetwork:
			outcome = "network_error"
		case ErrorThrottled:
			outcome = "throttled"
		case ErrorReputation:
			outcome = "reputation_error"
		default:
			outcome = "unknown_error"
		}
	} else {
		outcome = "unknown"
	}

	duration := float64(result.DurationMs) / 1000.0 // Convert to seconds
	d.metrics.RecordDeliveryAttempt(outcome, duration)
}
