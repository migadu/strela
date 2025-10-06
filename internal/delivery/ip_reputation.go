package delivery

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"fune/internal/config"

	"go.uber.org/zap"
)

// IPState represents the health state of a source IP
type IPState string

const (
	IPStateHealthy  IPState = "healthy"
	IPStateDegraded IPState = "degraded"
)

// DegradedIPInfo tracks information about a degraded IP
type DegradedIPInfo struct {
	IP               string
	DegradedAt       time.Time
	RetryAfter       time.Time
	FailureCount     int
	LastFailureError string
	LastSMTPCode     int
	LastSMTPResponse string
}

// ReputationAlert is sent to the configured webhook when an IP is degraded
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

// ReputationMetrics interface for recording IP reputation metrics
type ReputationMetrics interface {
	SetIPReputationDegraded(sourceIP string, degraded bool)
	RecordIPReputationEvent(eventType, sourceIP string)
}

// IPReputationTracker manages IP reputation and degraded states
type IPReputationTracker struct {
	mu          sync.RWMutex
	degradedIPs map[string]*DegradedIPInfo
	config      *config.ReputationConfig
	logger      *zap.Logger
	httpClient  *http.Client
	enabled     bool
	metrics     ReputationMetrics
}

// NewIPReputationTracker creates a new IP reputation tracker
func NewIPReputationTracker(cfg *config.ReputationConfig, logger *zap.Logger) *IPReputationTracker {
	enabled := cfg.EnableIPTracking
	if !enabled {
		logger.Warn("IP reputation tracking is DISABLED")
	} else {
		logger.Info("IP reputation tracking enabled",
			zap.Int("degraded_retry_hours", cfg.DegradedRetryHours),
			zap.String("alert_webhook_url", cfg.AlertWebhookURL))
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

// IsIPHealthy checks if an IP is in healthy state (not degraded or past retry time)
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
			zap.String("ip", ip),
			zap.Time("retry_after", info.RetryAfter))
		return true // Retry time reached, allow attempt
	}

	return false
}

// MarkIPDegraded marks an IP as degraded due to reputation failure
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
		zap.String("ip", ip),
		zap.Int("smtp_code", smtpCode),
		zap.String("smtp_response", smtpResponse),
		zap.Time("retry_after", retryAfter),
		zap.Int("failure_count", failureCount),
		zap.Int("total_degraded_ips", degradedCount))

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

// MarkIPRecovered marks a degraded IP as recovered after successful delivery
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
		zap.String("ip", ip),
		zap.Duration("degraded_duration", time.Since(degradedAt)),
		zap.Int("total_failures", failureCount),
		zap.Int("remaining_degraded_ips", degradedCount))

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

// RecordDeliveryAttempt records the result of a delivery attempt for IP tracking
func (rt *IPReputationTracker) RecordDeliveryAttempt(ip string, success bool, err *DeliveryError, deliveryInfo DeliveryInfo) {
	if !rt.enabled || ip == "" {
		return
	}

	// Check if IP was degraded
	rt.mu.RLock()
	_, wasDegraded := rt.degradedIPs[ip]
	rt.mu.RUnlock()

	if success && wasDegraded {
		// IP was degraded but now succeeded - mark as recovered
		rt.MarkIPRecovered(ip)
	} else if !success && err != nil && err.Category == ErrorReputation {
		// Reputation error - mark IP as degraded
		rt.MarkIPDegraded(ip, err.SMTPCode, err.SMTPResponse, deliveryInfo)
	}
}

// GetDegradedIPs returns a list of all currently degraded IPs
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

// Cleanup removes old degraded IP entries beyond the cleanup threshold
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
				zap.String("ip", ip),
				zap.Duration("age", time.Since(info.DegradedAt)))
		}
	}

	if removed > 0 {
		rt.logger.Info("degraded IP cleanup completed",
			zap.Int("removed", removed),
			zap.Int("remaining", len(rt.degradedIPs)))
	}
}

// GetHealthyIPs filters a list of IPs to return only healthy ones
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
			zap.Int("total_ips", len(ips)),
			zap.Int("healthy_ips", len(healthy)),
			zap.Int("degraded_ips", len(ips)-len(healthy)))
	}

	return healthy
}

// sendAlert sends a reputation alert to the configured webhook
func (rt *IPReputationTracker) sendAlert(alert ReputationAlert) {
	if rt.config.AlertWebhookURL == "" {
		return // No webhook configured
	}

	// Send in background to not block delivery
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Duration(rt.config.AlertTimeoutSeconds)*time.Second)
		defer cancel()

		jsonData, err := json.Marshal(alert)
		if err != nil {
			rt.logger.Error("failed to marshal reputation alert",
				zap.String("ip", alert.SourceIP),
				zap.Error(err))
			return
		}

		req, err := http.NewRequestWithContext(ctx, "POST", rt.config.AlertWebhookURL, bytes.NewReader(jsonData))
		if err != nil {
			rt.logger.Error("failed to create reputation alert request",
				zap.String("ip", alert.SourceIP),
				zap.Error(err))
			return
		}

		req.Header.Set("Content-Type", "application/json")
		if rt.config.AlertAuthToken != "" {
			req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", rt.config.AlertAuthToken))
		}

		resp, err := rt.httpClient.Do(req)
		if err != nil {
			rt.logger.Error("failed to send reputation alert",
				zap.String("ip", alert.SourceIP),
				zap.String("event_type", alert.EventType),
				zap.Error(err))
			return
		}
		defer resp.Body.Close()

		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			rt.logger.Info("reputation alert sent successfully",
				zap.String("ip", alert.SourceIP),
				zap.String("event_type", alert.EventType),
				zap.Int("status_code", resp.StatusCode))
		} else {
			rt.logger.Warn("reputation alert returned non-2xx status",
				zap.String("ip", alert.SourceIP),
				zap.String("event_type", alert.EventType),
				zap.Int("status_code", resp.StatusCode))
		}
	}()
}

// DeliveryInfo contains context about a delivery for reputation tracking
type DeliveryInfo struct {
	From           string
	To             string
	Subject        string
	IdempotencyKey string
	MXHost         string
}

// SetMetrics sets the metrics recorder for reputation tracking
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
