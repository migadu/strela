package delivery

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"fune/internal/config"
)

func TestNewDeliverer(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := &config.OutboundConfig{
		SourceIPSelection:        "round-robin",
		ConnectionTimeoutSeconds: 1,
		SMTPTimeoutSeconds:       1,
		DeliveryTimeoutSeconds:   1,
		PerDomainIntervalSeconds: 1,
	}
	dnsCfg := &config.DNSConfig{
		TimeoutSeconds: 1,
	}
	repCfg := &config.ReputationConfig{}
	expandedIPs := &config.ExpandedSourceIPs{
		IPv4: []string{},
		IPv6: []string{},
	}

	mxLookup := NewMXLookup(dnsCfg, logger)
	deliverer := NewDeliverer(cfg, expandedIPs, mxLookup, logger, repCfg, nil, nil)

	if deliverer == nil {
		t.Fatal("Deliverer should not be nil")
	}
}

func TestDeliverMessage_InvalidEmail(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := &config.OutboundConfig{}
	dnsCfg := &config.DNSConfig{}
	repCfg := &config.ReputationConfig{}
	expandedIPs := &config.ExpandedSourceIPs{
		IPv4: []string{},
		IPv6: []string{},
	}

	mxLookup := NewMXLookup(dnsCfg, logger)
	deliverer := NewDeliverer(cfg, expandedIPs, mxLookup, logger, repCfg, nil, nil)

	ctx := context.Background()
	result := deliverer.DeliverMessage(ctx, "sender@example.com", "invalid-recipient", []byte("test"), "", "", "", false, "", "", "")

	if result.Status != "hard_bounce" {
		t.Errorf("Expected hard_bounce for invalid recipient, got %s", result.Status)
	}
}

func TestDomainWhitelist(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := &config.OutboundConfig{
		PerDomainIntervalSeconds: 2,
		PerDomainBurst:           1,
		RateLimitWhitelist:       []string{"whitelisted.com", "trusted.net"},
	}
	dnsCfg := &config.DNSConfig{}
	repCfg := &config.ReputationConfig{}
	expandedIPs := &config.ExpandedSourceIPs{
		IPv4: []string{},
		IPv6: []string{},
	}

	mxLookup := NewMXLookup(dnsCfg, logger)
	deliverer := NewDeliverer(cfg, expandedIPs, mxLookup, logger, repCfg, nil, nil)

	tests := []struct {
		name               string
		domain             string
		expectWhitelisted  bool
	}{
		{
			name:              "whitelisted domain",
			domain:            "whitelisted.com",
			expectWhitelisted: true,
		},
		{
			name:              "whitelisted domain case insensitive",
			domain:            "WHITELISTED.COM",
			expectWhitelisted: true,
		},
		{
			name:              "second whitelisted domain",
			domain:            "trusted.net",
			expectWhitelisted: true,
		},
		{
			name:              "non-whitelisted domain",
			domain:            "example.com",
			expectWhitelisted: false,
		},
		{
			name:              "partial match not whitelisted",
			domain:            "sub.whitelisted.com",
			expectWhitelisted: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := deliverer.isDomainWhitelisted(tt.domain)
			if result != tt.expectWhitelisted {
				t.Errorf("isDomainWhitelisted(%q) = %v, want %v", tt.domain, result, tt.expectWhitelisted)
			}
		})
	}
}

func TestDomainWhitelist_EmptyList(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := &config.OutboundConfig{
		PerDomainIntervalSeconds: 2,
		PerDomainBurst:           1,
		RateLimitWhitelist:       []string{},
	}
	dnsCfg := &config.DNSConfig{}
	repCfg := &config.ReputationConfig{}
	expandedIPs := &config.ExpandedSourceIPs{
		IPv4: []string{},
		IPv6: []string{},
	}

	mxLookup := NewMXLookup(dnsCfg, logger)
	deliverer := NewDeliverer(cfg, expandedIPs, mxLookup, logger, repCfg, nil, nil)

	if deliverer.isDomainWhitelisted("any-domain.com") {
		t.Error("Empty whitelist should not match any domain")
	}
}
