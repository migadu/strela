package delivery

import (
	"testing"

	"strela/internal/config"
)

func TestIPRotator_OnlyIPv4(t *testing.T) {
	ipsV4 := []string{"192.0.2.1", "192.0.2.2"}
	ipsV6 := []string{} // No IPv6

	rotator := NewIPRotator(ipsV4, ipsV6, "round-robin", true) // prefer IPv6 but none available

	if !rotator.HasIPv4() {
		t.Error("Should have IPv4 addresses")
	}
	if rotator.HasIPv6() {
		t.Error("Should not have IPv6 addresses")
	}

	// Should not prefer IPv6 when none available
	if rotator.PreferIPv6() && !rotator.HasIPv6() {
		// This is OK - config says prefer but none available, will try IPv4
		t.Log("Prefer IPv6 set but no IPv6 available - will use IPv4")
	}

	allV4 := rotator.GetAllIPsV4()
	if len(allV4) != 2 {
		t.Errorf("Expected 2 IPv4 addresses, got %d", len(allV4))
	}

	allV6 := rotator.GetAllIPsV6()
	if len(allV6) != 0 {
		t.Errorf("Expected 0 IPv6 addresses, got %d", len(allV6))
	}
}

func TestIPRotator_OnlyIPv6(t *testing.T) {
	ipsV4 := []string{} // No IPv4
	ipsV6 := []string{"2001:db8::1", "2001:db8::2"}

	rotator := NewIPRotator(ipsV4, ipsV6, "round-robin", true)

	if rotator.HasIPv4() {
		t.Error("Should not have IPv4 addresses")
	}
	if !rotator.HasIPv6() {
		t.Error("Should have IPv6 addresses")
	}

	allV4 := rotator.GetAllIPsV4()
	if len(allV4) != 0 {
		t.Errorf("Expected 0 IPv4 addresses, got %d", len(allV4))
	}

	allV6 := rotator.GetAllIPsV6()
	if len(allV6) != 2 {
		t.Errorf("Expected 2 IPv6 addresses, got %d", len(allV6))
	}
}

func TestIPRotator_BothIPVersions(t *testing.T) {
	ipsV4 := []string{"192.0.2.1"}
	ipsV6 := []string{"2001:db8::1"}

	rotator := NewIPRotator(ipsV4, ipsV6, "round-robin", true)

	if !rotator.HasIPv4() {
		t.Error("Should have IPv4 addresses")
	}
	if !rotator.HasIPv6() {
		t.Error("Should have IPv6 addresses")
	}
	if !rotator.PreferIPv6() {
		t.Error("Should prefer IPv6")
	}
}

func TestIPRotator_NoIPs(t *testing.T) {
	ipsV4 := []string{}
	ipsV6 := []string{}

	rotator := NewIPRotator(ipsV4, ipsV6, "round-robin", false)

	if rotator.HasIPv4() {
		t.Error("Should not have IPv4 addresses")
	}
	if rotator.HasIPv6() {
		t.Error("Should not have IPv6 addresses")
	}
}

func TestExpandSourceIPs_OnlyIPv4Config(t *testing.T) {
	cfg := &config.OutboundConfig{
		SourceIPsV4: []string{"192.0.2.1", "192.0.2.2"},
		SourceIPsV6: []string{}, // Empty
		PreferIPv6:  false,
	}

	expandedV4, err := config.ExpandSourceIPs(cfg.SourceIPsV4)
	if err != nil {
		t.Fatalf("Failed to expand IPv4: %v", err)
	}

	expandedV6, err := config.ExpandSourceIPs(cfg.SourceIPsV6)
	if err != nil {
		t.Fatalf("Failed to expand IPv6: %v", err)
	}

	if len(expandedV4.IPv4) != 2 {
		t.Errorf("Expected 2 IPv4 addresses, got %d", len(expandedV4.IPv4))
	}
	if len(expandedV6.IPv6) != 0 {
		t.Errorf("Expected 0 IPv6 addresses, got %d", len(expandedV6.IPv6))
	}
}

func TestExpandSourceIPs_OnlyIPv6Config(t *testing.T) {
	cfg := &config.OutboundConfig{
		SourceIPsV4: []string{}, // Empty
		SourceIPsV6: []string{"2001:db8::1", "2001:db8::2"},
		PreferIPv6:  true,
	}

	expandedV4, err := config.ExpandSourceIPs(cfg.SourceIPsV4)
	if err != nil {
		t.Fatalf("Failed to expand IPv4: %v", err)
	}

	expandedV6, err := config.ExpandSourceIPs(cfg.SourceIPsV6)
	if err != nil {
		t.Fatalf("Failed to expand IPv6: %v", err)
	}

	if len(expandedV4.IPv4) != 0 {
		t.Errorf("Expected 0 IPv4 addresses, got %d", len(expandedV4.IPv4))
	}
	if len(expandedV6.IPv6) != 2 {
		t.Errorf("Expected 2 IPv6 addresses, got %d", len(expandedV6.IPv6))
	}
}
