package delivery

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"strela/internal/config"
	"strela/internal/recovery"
)

// IPState represents the health state of a source IP address.
type IPState string

const (
	// IPStateHealthy indicates the IP is in good standing and available for delivery.
	IPStateHealthy IPState = "healthy"
	// IPStateDegraded indicates the IP is blacklisted or has poor reputation and should be avoided.
	IPStateDegraded IPState = "degraded"
)

// DegradedIPInfo tracks detailed information about a degraded IP address,
// including when it was degraded, when to retry, failure counts, and the
// SMTP error that triggered the degradation.
type DegradedIPInfo struct {
	IP               string
	DegradedAt       time.Time
	RetryAfter       time.Time
	FailureCount     int
	LastFailureError string
	LastSMTPCode     int
	LastSMTPResponse string
}

// ReputationAlert is sent to the configured webhook when an IP is degraded or recovered.
// It contains full context about the event, including the message that triggered the
// degradation (for "degraded" events) and the count of currently degraded IPs.
type ReputationAlert struct {
	Timestamp        time.Time `json:"timestamp"`
	SourceIP         string    `json:"source_ip"`
	EventType        string    `json:"event_type"` // "degraded" or "recovered"
	From             string    `json:"from,omitempty"`
	To               string    `json:"to,omitempty"`
	Subject          string    `json:"subject,omitempty"`
	IdempotencyKey   string    `json:"idempotency_key,omitempty"`
	SMTPCode         int       `json:"smtp_code,omitempty"`
	SMTPResponse     string    `json:"smtp_response,omitempty"`
	MXHost           string    `json:"mx_host,omitempty"`
	RetryAfter       time.Time `json:"retry_after,omitempty"`
	DegradedIPsCount int       `json:"degraded_ips_count,omitempty"`
}

// ReputationMetrics defines the interface for recording IP reputation metrics
// to Prometheus or other monitoring systems.
type ReputationMetrics interface {
	SetIPReputationDegraded(sourceIP string, degraded bool)
	RecordIPReputationEvent(eventType, sourceIP string)
}

// IPReputationTracker manages IP reputation and degraded states for all source IPs.
// When an IP receives reputation errors (blacklist, poor reputation), it's marked as
// degraded and removed from the rotation pool. After a configured retry period, the
// IP is allowed to attempt delivery again. If successful, it's marked as recovered.
// The tracker can be disabled via configuration, in which case all IPs are considered healthy.
type IPReputationTracker struct {
	mu          sync.RWMutex
	degradedIPs map[string]*DegradedIPInfo
	config      *config.ReputationConfig
	logger      *slog.Logger
	httpClient  *http.Client
	enabled     bool
	metrics     ReputationMetrics
}

// NewIPReputationTracker creates a new IP reputation tracker with the specified configuration.
// If IP tracking is disabled in the config, all IPs will be considered healthy.
// The tracker uses an HTTP client with configurable timeout for sending webhook alerts.
func NewIPReputationTracker(cfg *config.ReputationConfig, logger *slog.Logger) *IPReputationTracker {
	enabled := cfg.EnableIPTracking
	if !enabled {
		logger.Warn("IP reputation tracking is DISABLED")
	} else {
		logger.Info("IP reputation tracking enabled",
			"degraded_retry_hours", cfg.DegradedRetryHours,
			"alert_webhook_url", cfg.AlertWebhookURL)
	}

	return &IPReputationTracker{
		degradedIPs: make(map[string]*DegradedIPInfo),
		config:      cfg,
		logger:      logger,
		httpClient: &http.Client{
			Timeout: time.Duration(cfg.AlertTimeoutSeconds) * time.Second,
		},
		enabled: enabled,
	}
}

// IsIPHealthy checks if an IP is in healthy state and can be used for delivery.
// An IP is considered healthy if it's not degraded or if the retry time has elapsed.
// If reputation tracking is disabled, all IPs are considered healthy.
func (rt *IPReputationTracker) IsIPHealthy(ip string) bool {
	if !rt.enabled {
		return true // If tracking disabled, all IPs are healthy
	}

	rt.mu.RLock()
	defer rt.mu.RUnlock()

	info, exists := rt.degradedIPs[ip]
	if !exists {
		return true // Not tracked, assume healthy
	}

	// Check if retry time has passed
	if time.Now().After(info.RetryAfter) {
		rt.logger.Debug("degraded IP retry time reached",
			"ip", ip,
			"retry_after", info.RetryAfter)
		return true // Retry time reached, allow attempt
	}

	rt.logger.Debug("IP is currently degraded", "ip", ip, "retry_after", info.RetryAfter)
	return false
}

// MarkIPDegraded marks an IP as degraded due to reputation failure (blacklist, poor reputation).
// The IP will be removed from rotation and retried after the configured period. A webhook
// alert is sent (if configured) with full context about the degradation. The failure count
// is incremented if the IP was already degraded.
func (rt *IPReputationTracker) MarkIPDegraded(ip string, smtpCode int, smtpResponse string, deliveryInfo DeliveryInfo) {
	if !rt.enabled {
		return
	}

	rt.mu.Lock()
	now := time.Now()
	retryAfter := now.Add(time.Duration(rt.config.DegradedRetryHours) * time.Hour)

	info, exists := rt.degradedIPs[ip]
	if !exists {
		info = &DegradedIPInfo{
			IP:           ip,
			DegradedAt:   now,
			RetryAfter:   retryAfter,
			FailureCount: 1,
		}
		rt.degradedIPs[ip] = info
	} else {
		info.FailureCount++
		info.DegradedAt = now
		info.RetryAfter = retryAfter
	}

	info.LastFailureError = "IP reputation/blacklist error"
	info.LastSMTPCode = smtpCode
	info.LastSMTPResponse = smtpResponse

	degradedCount := len(rt.degradedIPs)
	failureCount := info.FailureCount // Capture before unlock
	rt.mu.Unlock()

	rt.logger.Warn("IP marked as degraded due to reputation failure",
		"ip", ip,
		"smtp_code", smtpCode,
		"smtp_response", smtpResponse,
		"retry_after", retryAfter,
		"failure_count", failureCount,
		"total_degraded_ips", degradedCount)

	// Record metrics
	if rt.metrics != nil {
		rt.metrics.SetIPReputationDegraded(ip, true)
		rt.metrics.RecordIPReputationEvent("degraded", ip)
	}

	// Send alert webhook
	rt.sendAlert(ReputationAlert{
		Timestamp:        now,
		SourceIP:         ip,
		EventType:        "degraded",
		From:             deliveryInfo.From,
		To:               deliveryInfo.To,
		Subject:          deliveryInfo.Subject,
		IdempotencyKey:   deliveryInfo.IdempotencyKey,
		SMTPCode:         smtpCode,
		SMTPResponse:     smtpResponse,
		MXHost:           deliveryInfo.MXHost,
		RetryAfter:       retryAfter,
		DegradedIPsCount: degradedCount,
	})
}

// MarkIPRecovered marks a degraded IP as recovered after successful delivery.
// The IP is removed from the degraded list and returned to the rotation pool.
// A webhook alert is sent (if configured) to notify of the recovery.
func (rt *IPReputationTracker) MarkIPRecovered(ip string) {
	if !rt.enabled {
		return
	}

	rt.mu.Lock()
	info, exists := rt.degradedIPs[ip]
	if !exists {
		rt.mu.Unlock()
		return
	}

	// Capture info before delete
	degradedAt := info.DegradedAt
	failureCount := info.FailureCount

	delete(rt.degradedIPs, ip)
	degradedCount := len(rt.degradedIPs)
	rt.mu.Unlock()

	rt.logger.Info("IP recovered from degraded state",
		"ip", ip,
		"degraded_duration", time.Since(degradedAt),
		"total_failures", failureCount,
		"remaining_degraded_ips", degradedCount)

	// Record metrics
	if rt.metrics != nil {
		rt.metrics.SetIPReputationDegraded(ip, false)
		rt.metrics.RecordIPReputationEvent("recovered", ip)
	}

	// Send recovery alert
	rt.sendAlert(ReputationAlert{
		Timestamp:        time.Now(),
		SourceIP:         ip,
		EventType:        "recovered",
		DegradedIPsCount: degradedCount,
	})
}

// RecordDeliveryAttempt records the result of a delivery attempt for IP reputation tracking.
// If the IP was degraded and the delivery succeeded, it's marked as recovered. If the
// delivery failed with a reputation error, the IP is marked as degraded. This method
// should be called after every delivery attempt with a source IP.
func (rt *IPReputationTracker) RecordDeliveryAttempt(ip string, success bool, err *DeliveryError, deliveryInfo DeliveryInfo) {
	if !rt.enabled || ip == "" {
		return
	}

	// Check if IP was degraded
	rt.mu.RLock()
	_, wasDegraded := rt.degradedIPs[ip]
	rt.mu.RUnlock()

	if success && wasDegraded {
		rt.logger.Debug("successful delivery with degraded IP, marking as recovered", "ip", ip)
		// IP was degraded but now succeeded - mark as recovered
		rt.MarkIPRecovered(ip)
	} else if !success && err != nil && err.Category == ErrorReputation {
		rt.logger.Debug("delivery failed with reputation error, marking IP as degraded", "ip", ip, "error", err)
		// Reputation error - mark IP as degraded
		rt.MarkIPDegraded(ip, err.SMTPCode, err.SMTPResponse, deliveryInfo)
	}
}

// GetDegradedIPs returns a copy of all currently degraded IPs with their information.
// The returned map is a copy to prevent external modification of the tracker's internal state.
func (rt *IPReputationTracker) GetDegradedIPs() map[string]*DegradedIPInfo {
	rt.mu.RLock()
	defer rt.mu.RUnlock()

	// Return a copy to prevent external modification
	result := make(map[string]*DegradedIPInfo, len(rt.degradedIPs))
	for ip, info := range rt.degradedIPs {
		infoCopy := *info
		result[ip] = &infoCopy
	}
	return result
}

// Cleanup removes old degraded IP entries that have been degraded beyond the cleanup
// threshold and whose retry time has passed. This prevents unbounded memory growth
// from IPs that are never recovered. Should be called periodically.
func (rt *IPReputationTracker) Cleanup() {
	if !rt.enabled {
		return
	}

	rt.mu.Lock()
	defer rt.mu.Unlock()

	cutoff := time.Now().Add(-time.Duration(rt.config.DegradedIPCleanupHours) * time.Hour)
	removed := 0

	for ip, info := range rt.degradedIPs {
		// Remove if degraded time is beyond cleanup threshold AND retry time has passed
		if info.DegradedAt.Before(cutoff) && time.Now().After(info.RetryAfter) {
			delete(rt.degradedIPs, ip)
			removed++
			rt.logger.Info("cleaned up old degraded IP entry",
				"ip", ip,
				"age", time.Since(info.DegradedAt))
		}
	}

	if removed > 0 {
		rt.logger.Info("degraded IP cleanup completed",
			"removed", removed,
			"remaining", len(rt.degradedIPs))
	}
}

// GetHealthyIPs filters a list of IPs to return only those in healthy state.
// Degraded IPs are filtered out unless their retry time has elapsed.
// If reputation tracking is disabled, all IPs are returned as-is.
func (rt *IPReputationTracker) GetHealthyIPs(ips []string) []string {
	if !rt.enabled {
		return ips
	}

	healthy := make([]string, 0, len(ips))
	for _, ip := range ips {
		if rt.IsIPHealthy(ip) {
			healthy = append(healthy, ip)
		}
	}

	if len(healthy) < len(ips) {
		rt.logger.Debug("filtered degraded IPs",
			"total_ips", len(ips),
			"healthy_ips", len(healthy),
			"degraded_ips", len(ips)-len(healthy))
	}

	return healthy
}

// sendAlert sends a reputation alert to the configured webhook
func (rt *IPReputationTracker) sendAlert(alert ReputationAlert) {
	if rt.config.AlertWebhookURL == "" {
		return // No webhook configured
	}

	// Send in background to not block delivery with panic recovery
	recovery.SafeGo(rt.logger, "reputation-alert", func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Duration(rt.config.AlertTimeoutSeconds)*time.Second)
		defer cancel()

		jsonData, err := json.Marshal(alert)
		if err != nil {
			rt.logger.Error("failed to marshal reputation alert",
				"ip", alert.SourceIP,
				"error", err)
			return
		}

		req, err := http.NewRequestWithContext(ctx, "POST", rt.config.AlertWebhookURL, bytes.NewReader(jsonData))
		if err != nil {
			rt.logger.Error("failed to create reputation alert request",
				"ip", alert.SourceIP,
				"error", err)
			return
		}

		req.Header.Set("Content-Type", "application/json")
		if rt.config.AlertAuthToken != "" {
			req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", rt.config.AlertAuthToken))
		}

		resp, err := rt.httpClient.Do(req)
		if err != nil {
			rt.logger.Error("failed to send reputation alert",
				"ip", alert.SourceIP,
				"event_type", alert.EventType,
				"error", err)
			return
		}
		defer resp.Body.Close()

		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			rt.logger.Info("reputation alert sent successfully",
				"ip", alert.SourceIP,
				"event_type", alert.EventType,
				"status_code", resp.StatusCode)
		} else {
			rt.logger.Warn("reputation alert returned non-2xx status",
				"ip", alert.SourceIP,
				"event_type", alert.EventType,
				"status_code", resp.StatusCode)
		}
	})
}

// DeliveryInfo contains contextual information about a delivery attempt,
// used for reputation tracking and webhook alerts.
type DeliveryInfo struct {
	From           string
	To             string
	Subject        string
	IdempotencyKey string
	MXHost         string
}

// SetMetrics sets the metrics recorder for reputation tracking, enabling Prometheus
// metrics for IP degradation events. It also sets the initial state for all currently
// degraded IPs.
func (rt *IPReputationTracker) SetMetrics(metrics ReputationMetrics) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	rt.metrics = metrics

	// Set initial state for all currently degraded IPs
	if metrics != nil {
		for ip := range rt.degradedIPs {
			metrics.SetIPReputationDegraded(ip, true)
		}
	}
}
