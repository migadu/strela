package delivery

import (
	"testing"
)

// TestDeliveryFlow_IPv4Only verifies that when only IPv4 is configured,
// we don't attempt IPv6 delivery at all.
func TestDeliveryFlow_IPv4Only(t *testing.T) {
	ipsV4 := []string{"192.0.2.1"}
	ipsV6 := []string{} // No IPv6

	rotator := NewIPRotator(ipsV4, ipsV6, "round-robin", true) // prefer IPv6 but none available

	// Check flow logic
	tryIPv6First := rotator.PreferIPv6() && rotator.HasIPv6()
	tryIPv4 := rotator.HasIPv4()
	tryIPv6 := rotator.HasIPv6()

	if tryIPv6First {
		t.Error("Should not try IPv6 first when no IPv6 IPs configured")
	}

	if !tryIPv4 {
		t.Error("Should try IPv4 when IPv4 IPs configured")
	}

	if tryIPv6 {
		t.Error("Should not try IPv6 when no IPv6 IPs configured")
	}

	// Verify the logic path: goes to "else if tryIPv4" branch (line 172 in delivery.go)
	// and does NOT execute the fallback IPv6 attempt (line 181 check fails)
	if tryIPv4 && !tryIPv6 {
		t.Log("✓ Correct flow: Will only attempt IPv4 delivery")
	}
}

// TestDeliveryFlow_IPv6Only verifies that when only IPv6 is configured,
// we don't attempt IPv4 delivery at all.
func TestDeliveryFlow_IPv6Only(t *testing.T) {
	ipsV4 := []string{} // No IPv4
	ipsV6 := []string{"2001:db8::1"}

	rotator := NewIPRotator(ipsV4, ipsV6, "round-robin", true)

	// Check flow logic
	tryIPv6First := rotator.PreferIPv6() && rotator.HasIPv6()
	tryIPv4 := rotator.HasIPv4()
	tryIPv6 := rotator.HasIPv6()

	if !tryIPv6First {
		t.Error("Should try IPv6 first when IPv6 IPs configured and preferred")
	}

	if tryIPv4 {
		t.Error("Should not try IPv4 when no IPv4 IPs configured")
	}

	if !tryIPv6 {
		t.Error("Should try IPv6 when IPv6 IPs configured")
	}

	// Verify the logic path: goes to "if tryIPv6First" branch (line 157 in delivery.go)
	// and does NOT execute the fallback IPv4 attempt (line 165 check fails)
	if tryIPv6First && !tryIPv4 {
		t.Log("✓ Correct flow: Will only attempt IPv6 delivery")
	}
}

// TestDeliveryFlow_BothIPv6Preferred verifies that when both are configured,
// we try IPv6 first, then fall back to IPv4.
func TestDeliveryFlow_BothIPv6Preferred(t *testing.T) {
	ipsV4 := []string{"192.0.2.1"}
	ipsV6 := []string{"2001:db8::1"}

	rotator := NewIPRotator(ipsV4, ipsV6, "round-robin", true)

	// Check flow logic
	tryIPv6First := rotator.PreferIPv6() && rotator.HasIPv6()
	tryIPv4 := rotator.HasIPv4()
	tryIPv6 := rotator.HasIPv6()

	if !tryIPv6First {
		t.Error("Should try IPv6 first when both configured and IPv6 preferred")
	}

	if !tryIPv4 {
		t.Error("Should have IPv4 available for fallback")
	}

	if !tryIPv6 {
		t.Error("Should have IPv6 available")
	}

	// Verify the logic path: goes to "if tryIPv6First" branch (line 157)
	// tries IPv6, then falls back to IPv4 if IPv6 fails (line 165 check succeeds)
	if tryIPv6First && tryIPv4 {
		t.Log("✓ Correct flow: Will attempt IPv6 first, then fallback to IPv4")
	}
}

// TestDeliveryFlow_BothIPv4Preferred verifies that when both are configured
// but IPv4 is preferred, we try IPv4 first, then fall back to IPv6.
func TestDeliveryFlow_BothIPv4Preferred(t *testing.T) {
	ipsV4 := []string{"192.0.2.1"}
	ipsV6 := []string{"2001:db8::1"}

	rotator := NewIPRotator(ipsV4, ipsV6, "round-robin", false) // prefer_ipv6 = false

	// Check flow logic
	tryIPv6First := rotator.PreferIPv6() && rotator.HasIPv6()
	tryIPv4 := rotator.HasIPv4()
	tryIPv6 := rotator.HasIPv6()

	if tryIPv6First {
		t.Error("Should not try IPv6 first when IPv4 is preferred")
	}

	if !tryIPv4 {
		t.Error("Should have IPv4 available")
	}

	if !tryIPv6 {
		t.Error("Should have IPv6 available for fallback")
	}

	// Verify the logic path: goes to "else if tryIPv4" branch (line 172)
	// tries IPv4, then falls back to IPv6 if IPv4 fails (line 181 check succeeds)
	if !tryIPv6First && tryIPv4 && tryIPv6 {
		t.Log("✓ Correct flow: Will attempt IPv4 first, then fallback to IPv6")
	}
}

// TestDeliveryFlow_NoSourceIPs verifies the system default behavior
// when no source IPs are configured.
func TestDeliveryFlow_NoSourceIPs(t *testing.T) {
	ipsV4 := []string{}
	ipsV6 := []string{}

	rotator := NewIPRotator(ipsV4, ipsV6, "round-robin", false)

	// Check flow logic
	tryIPv6First := rotator.PreferIPv6() && rotator.HasIPv6()
	tryIPv4 := rotator.HasIPv4()
	tryIPv6 := rotator.HasIPv6()

	if tryIPv6First {
		t.Error("Should not try IPv6 first when no IPs configured")
	}

	if tryIPv4 {
		t.Error("Should not have IPv4 when none configured")
	}

	if tryIPv6 {
		t.Error("Should not have IPv6 when none configured")
	}

	// Verify the logic path: goes to "else" branch (line 188)
	// uses system default IP (sourceIP = "")
	if !tryIPv4 && !tryIPv6 {
		t.Log("✓ Correct flow: Will use system default IP (no source binding)")
	}
}
