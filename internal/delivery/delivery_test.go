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
	result := deliverer.DeliverMessage(ctx, "sender@example.com", "invalid-recipient", []byte("test"))

	if result.Status != "hard_bounce" {
		t.Errorf("Expected hard_bounce for invalid recipient, got %s", result.Status)
	}
}
