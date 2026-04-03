package delivery

import (
	"testing"

	"strela/internal/config"
)

func TestIPRotator_OnlyIPv4(t *testing.T) {
	ipsV4 := []string{"192.0.2.1", "192.0.2.2"}
	ipsV6 := []string{} // No IPv6

	rotator := NewIPRotator(ipsV4, ipsV6, "round-robin")

	if !rotator.HasIPv4() {
		t.Error("Should have IPv4 addresses")
	}
	if rotator.HasIPv6() {
		t.Error("Should not have IPv6 addresses")
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

	rotator := NewIPRotator(ipsV4, ipsV6, "round-robin")

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

	rotator := NewIPRotator(ipsV4, ipsV6, "round-robin")

	if !rotator.HasIPv4() {
		t.Error("Should have IPv4 addresses")
	}
	if !rotator.HasIPv6() {
		t.Error("Should have IPv6 addresses")
	}
}

func TestIPRotator_NoIPs(t *testing.T) {
	ipsV4 := []string{}
	ipsV6 := []string{}

	rotator := NewIPRotator(ipsV4, ipsV6, "round-robin")

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
		SourceIPsV6: []string{},
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
		SourceIPsV4: []string{},
		SourceIPsV6: []string{"2001:db8::1", "2001:db8::2"},
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
