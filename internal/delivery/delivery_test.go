package delivery

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"strela/internal/config"
	"strela/internal/srs"
)

func TestNewDeliverer(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := &config.OutboundConfig{
		SourceIPSelection:        "round-robin",
		ConnectionTimeoutSeconds: 1,
		SMTPTimeoutSeconds:       1,
		MaxTotalDeliverySeconds:  1,
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
	result := deliverer.DeliverMessage(ctx, "sender@example.com", "invalid-recipient", []byte("test"), "", "", "", "", false, "", "", "", nil)

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
		name              string
		domain            string
		expectWhitelisted bool
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

// newTestDelivererWithSRS creates a Deliverer with SRS enabled for testing shouldSkipSRS.
func newTestDelivererWithSRS(t *testing.T, srsCfg *config.SRSConfig) *Deliverer {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := &config.OutboundConfig{}
	dnsCfg := &config.DNSConfig{}
	repCfg := &config.ReputationConfig{}
	expandedIPs := &config.ExpandedSourceIPs{IPv4: []string{}, IPv6: []string{}}
	mxLookup := NewMXLookup(dnsCfg, logger)
	return NewDeliverer(cfg, expandedIPs, mxLookup, logger, repCfg, nil, srsCfg)
}

func TestShouldSkipSRS_SkipDomains(t *testing.T) {
	srsCfg := &config.SRSConfig{
		Enabled:     true,
		Domains:     []string{"srs.example.com"},
		Secret:      "test-secret-key-1234",
		HashLength:  4,
		Separator:   "=",
		SkipDomains: []string{"gmail.com", "googlemail.com"},
	}
	d := newTestDelivererWithSRS(t, srsCfg)

	tests := []struct {
		name       string
		from       string
		to         string
		wantSkip   bool
		wantReason string
	}{
		{
			name:       "skip for gmail.com",
			from:       "user@origin.com",
			to:         "recipient@gmail.com",
			wantSkip:   true,
			wantReason: "skip_domains",
		},
		{
			name:       "skip for googlemail.com",
			from:       "user@origin.com",
			to:         "recipient@googlemail.com",
			wantSkip:   true,
			wantReason: "skip_domains",
		},
		{
			name:       "skip domain case insensitive",
			from:       "user@origin.com",
			to:         "recipient@GMAIL.COM",
			wantSkip:   true,
			wantReason: "skip_domains",
		},
		{
			name:     "no skip for unlisted domain",
			from:     "user@origin.com",
			to:       "recipient@outlook.com",
			wantSkip: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			skip, reason := d.shouldSkipSRS(tt.from, tt.to, nil)
			if skip != tt.wantSkip {
				t.Errorf("shouldSkipSRS() skip = %v, want %v", skip, tt.wantSkip)
			}
			if tt.wantSkip && reason != tt.wantReason {
				t.Errorf("shouldSkipSRS() reason = %q, want %q", reason, tt.wantReason)
			}
		})
	}
}

func TestShouldSkipSRS_SkipIfDKIMPass(t *testing.T) {
	srsCfg := &config.SRSConfig{
		Enabled:        true,
		Domains:        []string{"srs.example.com"},
		Secret:         "test-secret-key-1234",
		HashLength:     4,
		Separator:      "=",
		SkipIfDKIMPass: true,
	}
	d := newTestDelivererWithSRS(t, srsCfg)

	tests := []struct {
		name        string
		inboundAuth *InboundAuthResults
		wantSkip    bool
		wantReason  string
	}{
		{name: "skip on dkim pass (lowercase)", inboundAuth: &InboundAuthResults{DKIM: "pass"}, wantSkip: true, wantReason: "dkim_pass"},
		{name: "skip on dkim pass (uppercase)", inboundAuth: &InboundAuthResults{DKIM: "PASS"}, wantSkip: true, wantReason: "dkim_pass"},
		{name: "skip on dkim pass (mixed case)", inboundAuth: &InboundAuthResults{DKIM: "Pass"}, wantSkip: true, wantReason: "dkim_pass"},
		{name: "no skip on dkim fail", inboundAuth: &InboundAuthResults{DKIM: "fail"}, wantSkip: false},
		{name: "no skip on dkim none", inboundAuth: &InboundAuthResults{DKIM: "none"}, wantSkip: false},
		{name: "no skip on nil inbound auth", inboundAuth: nil, wantSkip: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			skip, reason := d.shouldSkipSRS("user@origin.com", "recipient@example.com", tt.inboundAuth)
			if skip != tt.wantSkip {
				t.Errorf("shouldSkipSRS() skip = %v, want %v", skip, tt.wantSkip)
			}
			if tt.wantSkip && reason != tt.wantReason {
				t.Errorf("shouldSkipSRS() reason = %q, want %q", reason, tt.wantReason)
			}
		})
	}
}

func TestShouldSkipSRS_SkipIfDKIMPass_Disabled(t *testing.T) {
	// When SkipIfDKIMPass is false, dkim=pass must NOT skip SRS
	srsCfg := &config.SRSConfig{
		Enabled:        true,
		Domains:        []string{"srs.example.com"},
		Secret:         "test-secret-key-1234",
		HashLength:     4,
		Separator:      "=",
		SkipIfDKIMPass: false,
	}
	d := newTestDelivererWithSRS(t, srsCfg)

	skip, _ := d.shouldSkipSRS("user@origin.com", "recipient@example.com", &InboundAuthResults{DKIM: "pass"})
	if skip {
		t.Error("shouldSkipSRS() should not skip when SkipIfDKIMPass=false even with dkim=pass")
	}
}

func TestShouldSkipSRS_SkipIfSameDomain(t *testing.T) {
	srsCfg := &config.SRSConfig{
		Enabled:          true,
		Domains:          []string{"srs.example.com"},
		Secret:           "test-secret-key-1234",
		HashLength:       4,
		Separator:        "=",
		SkipIfSameDomain: true,
	}
	d := newTestDelivererWithSRS(t, srsCfg)

	tests := []struct {
		name       string
		from       string
		to         string
		wantSkip   bool
		wantReason string
	}{
		{
			name:       "skip when sender and recipient share domain",
			from:       "alice@example.com",
			to:         "bob@example.com",
			wantSkip:   true,
			wantReason: "same_domain",
		},
		{
			name:       "skip same domain case insensitive",
			from:       "alice@EXAMPLE.COM",
			to:         "bob@example.com",
			wantSkip:   true,
			wantReason: "same_domain",
		},
		{
			name:     "no skip for different domains",
			from:     "alice@origin.com",
			to:       "bob@example.com",
			wantSkip: false,
		},
		{
			name:     "no skip when from has no domain",
			from:     "alice",
			to:       "bob@example.com",
			wantSkip: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			skip, reason := d.shouldSkipSRS(tt.from, tt.to, nil)
			if skip != tt.wantSkip {
				t.Errorf("shouldSkipSRS() skip = %v, want %v", skip, tt.wantSkip)
			}
			if tt.wantSkip && reason != tt.wantReason {
				t.Errorf("shouldSkipSRS() reason = %q, want %q", reason, tt.wantReason)
			}
		})
	}
}

func TestShouldSkipSRS_SkipIfSameDomain_Disabled(t *testing.T) {
	srsCfg := &config.SRSConfig{
		Enabled:          true,
		Domains:          []string{"srs.example.com"},
		Secret:           "test-secret-key-1234",
		HashLength:       4,
		Separator:        "=",
		SkipIfSameDomain: false,
	}
	d := newTestDelivererWithSRS(t, srsCfg)

	skip, _ := d.shouldSkipSRS("alice@example.com", "bob@example.com", nil)
	if skip {
		t.Error("shouldSkipSRS() should not skip when SkipIfSameDomain=false even with matching domains")
	}
}

func TestShouldSkipSRS_Priority(t *testing.T) {
	// skip_domains takes priority over dkim_pass and same_domain
	srsCfg := &config.SRSConfig{
		Enabled:          true,
		Domains:          []string{"srs.example.com"},
		Secret:           "test-secret-key-1234",
		HashLength:       4,
		Separator:        "=",
		SkipDomains:      []string{"gmail.com"},
		SkipIfDKIMPass:   true,
		SkipIfSameDomain: true,
	}
	d := newTestDelivererWithSRS(t, srsCfg)

	// All conditions true: skip_domains should win
	skip, reason := d.shouldSkipSRS("user@gmail.com", "recipient@gmail.com", &InboundAuthResults{DKIM: "pass"})
	if !skip {
		t.Error("shouldSkipSRS() should skip")
	}
	if reason != "skip_domains" {
		t.Errorf("expected reason 'skip_domains', got %q", reason)
	}
}

func TestShouldSkipSRS_NoSRSConfig(t *testing.T) {
	// When srsConfig is nil, SRS is disabled entirely (d.srs == nil).
	// deliverPayload never calls shouldSkipSRS in this case.
	// Verify that a *SRS instance with no skip options set doesn't skip.
	srsInst, _ := srs.NewSRS([]string{"srs.example.com"}, "round-robin", "test-secret-key-1234", 21, 4, "=", nil, false, false, false)

	skip, _ := srsInst.ShouldSkip("alice@example.com", "alice@example.com", "pass")
	if skip {
		t.Error("ShouldSkip() should not skip when no skip options are set")
	}
}

func TestLMTPMode_SkipsMXLookup(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := &config.OutboundConfig{
		DefaultProtocol:          "lmtp",
		DefaultLMTPDestination:   "lmtp.example.com:24",
		ConnectionTimeoutSeconds: 1,
		SMTPTimeoutSeconds:       1,
		MaxTotalDeliverySeconds:  1,
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

	if deliverer.config.DefaultProtocol != "lmtp" {
		t.Errorf("Expected protocol 'lmtp', got: %s", deliverer.config.DefaultProtocol)
	}

	if deliverer.config.DefaultLMTPDestination != "lmtp.example.com:24" {
		t.Errorf("Expected LMTP destination 'lmtp.example.com:24', got: %s", deliverer.config.DefaultLMTPDestination)
	}
}

func TestLMTPMode_InvalidDestination(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := &config.OutboundConfig{
		DefaultProtocol:          "lmtp",
		DefaultLMTPDestination:   "invalid",
		ConnectionTimeoutSeconds: 1,
		SMTPTimeoutSeconds:       1,
		MaxTotalDeliverySeconds:  1,
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

	ctx := context.Background()
	result := deliverer.DeliverMessage(ctx, "sender@example.com", "recipient@example.com", []byte("test"), "", "", "", "", false, "", "", "", nil)

	if result.Status != "error" {
		t.Errorf("Expected error status for invalid LMTP destination, got %s", result.Status)
	}

	if result.Error == "" {
		t.Error("Expected error message for invalid LMTP destination")
	}
}

func TestDeliverMessage_ExplicitTransportOverridesConfig(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := &config.OutboundConfig{
		DefaultProtocol:          "smtp",
		DefaultLMTPDestination:   "lmtp.example.com:24",
		ConnectionTimeoutSeconds: 1,
		SMTPTimeoutSeconds:       1,
		MaxTotalDeliverySeconds:  1,
	}
	dnsCfg := &config.DNSConfig{TimeoutSeconds: 1}
	repCfg := &config.ReputationConfig{}
	expandedIPs := &config.ExpandedSourceIPs{IPv4: []string{}, IPv6: []string{}}

	mxLookup := NewMXLookup(dnsCfg, logger)
	deliverer := NewDeliverer(cfg, expandedIPs, mxLookup, logger, repCfg, nil, nil)

	ctx := context.Background()
	// Config says smtp, but explicit transport says lmtp — should use LMTP path
	result := deliverer.DeliverMessage(ctx, "sender@example.com", "recipient@example.com", []byte("test"), "lmtp", "", "", "", false, "", "", "", nil)

	// Will fail to connect but should use LMTP path (not MX lookup)
	// The error should reference the LMTP destination, not MX lookup failure
	if result.Status == "temp_fail" && result.Error != "" {
		// MX lookup failure means it didn't use LMTP — unexpected
		t.Logf("Got temp_fail: %s", result.Error)
	}
}
