package delivery

import (
	"testing"

	"strela/internal/config"
)

// TestDeliveryFlow_IPv4Only verifies that when ip_mode is "ipv4",
// we don't attempt IPv6 delivery at all.
func TestDeliveryFlow_IPv4Only(t *testing.T) {
	ipsV4 := []string{"192.0.2.1"}
	ipsV6 := []string{"2001:db8::1"} // Available but should not be used

	rotator := NewIPRotator(ipsV4, ipsV6, "round-robin")
	cfg := &config.OutboundConfig{SMTPIPMode: config.IPModeIPv4}

	ipMode := cfg.IPModeForProtocol(config.ProtocolSMTP)
	tryIPv4 := (ipMode == config.IPModeDual || ipMode == config.IPModeIPv4) && rotator.HasIPv4()
	tryIPv6 := (ipMode == config.IPModeDual || ipMode == config.IPModeIPv6) && rotator.HasIPv6()
	tryIPv6First := tryIPv6 && (ipMode == config.IPModeDual || ipMode == config.IPModeIPv6)

	if tryIPv6First {
		t.Error("Should not try IPv6 first in ipv4 mode")
	}
	if !tryIPv4 {
		t.Error("Should try IPv4 in ipv4 mode")
	}
	if tryIPv6 {
		t.Error("Should not try IPv6 in ipv4 mode")
	}
}

// TestDeliveryFlow_IPv6Only verifies that when ip_mode is "ipv6",
// we don't attempt IPv4 delivery at all.
func TestDeliveryFlow_IPv6Only(t *testing.T) {
	ipsV4 := []string{"192.0.2.1"} // Available but should not be used
	ipsV6 := []string{"2001:db8::1"}

	rotator := NewIPRotator(ipsV4, ipsV6, "round-robin")
	cfg := &config.OutboundConfig{SMTPIPMode: config.IPModeIPv6}

	ipMode := cfg.IPModeForProtocol(config.ProtocolSMTP)
	tryIPv4 := (ipMode == config.IPModeDual || ipMode == config.IPModeIPv4) && rotator.HasIPv4()
	tryIPv6 := (ipMode == config.IPModeDual || ipMode == config.IPModeIPv6) && rotator.HasIPv6()
	tryIPv6First := tryIPv6 && (ipMode == config.IPModeDual || ipMode == config.IPModeIPv6)

	if !tryIPv6First {
		t.Error("Should try IPv6 first in ipv6 mode")
	}
	if tryIPv4 {
		t.Error("Should not try IPv4 in ipv6 mode")
	}
	if !tryIPv6 {
		t.Error("Should try IPv6 in ipv6 mode")
	}
}

// TestDeliveryFlow_Dual verifies that when ip_mode is "dual",
// we try IPv6 first, then fall back to IPv4.
func TestDeliveryFlow_Dual(t *testing.T) {
	ipsV4 := []string{"192.0.2.1"}
	ipsV6 := []string{"2001:db8::1"}

	rotator := NewIPRotator(ipsV4, ipsV6, "round-robin")
	cfg := &config.OutboundConfig{SMTPIPMode: config.IPModeDual}

	ipMode := cfg.IPModeForProtocol(config.ProtocolSMTP)
	tryIPv4 := (ipMode == config.IPModeDual || ipMode == config.IPModeIPv4) && rotator.HasIPv4()
	tryIPv6 := (ipMode == config.IPModeDual || ipMode == config.IPModeIPv6) && rotator.HasIPv6()
	tryIPv6First := tryIPv6 && (ipMode == config.IPModeDual || ipMode == config.IPModeIPv6)

	if !tryIPv6First {
		t.Error("Should try IPv6 first in dual mode")
	}
	if !tryIPv4 {
		t.Error("Should have IPv4 available for fallback in dual mode")
	}
	if !tryIPv6 {
		t.Error("Should have IPv6 available in dual mode")
	}
}

// TestDeliveryFlow_PerProtocol verifies that SMTP and LMTP can have
// independent IP mode settings.
func TestDeliveryFlow_PerProtocol(t *testing.T) {
	ipsV4 := []string{"192.0.2.1"}
	ipsV6 := []string{"2001:db8::1"}

	rotator := NewIPRotator(ipsV4, ipsV6, "round-robin")
	cfg := &config.OutboundConfig{
		SMTPIPMode: config.IPModeDual,
		LMTPIPMode: config.IPModeIPv4,
	}

	// SMTP should use dual
	smtpMode := cfg.IPModeForProtocol(config.ProtocolSMTP)
	smtpTryIPv6 := (smtpMode == config.IPModeDual || smtpMode == config.IPModeIPv6) && rotator.HasIPv6()
	if !smtpTryIPv6 {
		t.Error("SMTP in dual mode should try IPv6")
	}

	// LMTP should use ipv4 only
	lmtpMode := cfg.IPModeForProtocol(config.ProtocolLMTP)
	lmtpTryIPv6 := (lmtpMode == config.IPModeDual || lmtpMode == config.IPModeIPv6) && rotator.HasIPv6()
	lmtpTryIPv4 := (lmtpMode == config.IPModeDual || lmtpMode == config.IPModeIPv4) && rotator.HasIPv4()
	if lmtpTryIPv6 {
		t.Error("LMTP in ipv4 mode should not try IPv6")
	}
	if !lmtpTryIPv4 {
		t.Error("LMTP in ipv4 mode should try IPv4")
	}
}

// TestDeliveryFlow_NoSourceIPs verifies the system default behavior
// when no source IPs are configured.
func TestDeliveryFlow_NoSourceIPs(t *testing.T) {
	ipsV4 := []string{}
	ipsV6 := []string{}

	rotator := NewIPRotator(ipsV4, ipsV6, "round-robin")
	cfg := &config.OutboundConfig{SMTPIPMode: config.IPModeDual}

	ipMode := cfg.IPModeForProtocol(config.ProtocolSMTP)
	tryIPv4 := (ipMode == config.IPModeDual || ipMode == config.IPModeIPv4) && rotator.HasIPv4()
	tryIPv6 := (ipMode == config.IPModeDual || ipMode == config.IPModeIPv6) && rotator.HasIPv6()

	if tryIPv4 {
		t.Error("Should not have IPv4 when none configured")
	}
	if tryIPv6 {
		t.Error("Should not have IPv6 when none configured")
	}
}
