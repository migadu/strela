package config

import (
	"fmt"
	"net"
)

// ExpandedSourceIPs holds expanded IPv4 and IPv6 source IPs after CIDR expansion.
type ExpandedSourceIPs struct {
	IPv4 []string
	IPv6 []string
}

// ExpandSourceIPs expands CIDR subnets into individual IP addresses and classifies them as IPv4 or IPv6.
// It accepts a mixed list of individual IPs and CIDR subnets (e.g., ["192.0.2.1", "192.0.2.0/24", "2001:db8::1"]).
// Returns an ExpandedSourceIPs struct with separate IPv4 and IPv6 lists.
func ExpandSourceIPs(sourceIPs []string) (*ExpandedSourceIPs, error) {
	result := &ExpandedSourceIPs{
		IPv4: make([]string, 0),
		IPv6: make([]string, 0),
	}

	for _, entry := range sourceIPs {
		// Check if it's a CIDR subnet
		if _, ipNet, err := net.ParseCIDR(entry); err == nil {
			// It's a CIDR subnet - expand it
			expanded, err := expandCIDR(ipNet)
			if err != nil {
				return nil, fmt.Errorf("failed to expand CIDR %s: %w", entry, err)
			}

			// Classify expanded IPs
			for _, expandedIP := range expanded {
				if isIPv4(expandedIP) {
					result.IPv4 = append(result.IPv4, expandedIP)
				} else {
					result.IPv6 = append(result.IPv6, expandedIP)
				}
			}
		} else {
			// It's a single IP address
			ip := net.ParseIP(entry)
			if ip == nil {
				return nil, fmt.Errorf("invalid IP address: %s", entry)
			}

			if isIPv4(ip.String()) {
				result.IPv4 = append(result.IPv4, ip.String())
			} else {
				result.IPv6 = append(result.IPv6, ip.String())
			}
		}
	}

	return result, nil
}

// expandCIDR expands a CIDR subnet into individual IP addresses.
// WARNING: This can be expensive for large subnets (e.g., /16 = 65536 IPs).
// For production use, consider limiting subnet sizes or implementing lazy expansion.
func expandCIDR(ipNet *net.IPNet) ([]string, error) {
	var ips []string

	// Get network and broadcast addresses
	ip := ipNet.IP.Mask(ipNet.Mask)

	// Calculate the number of hosts
	ones, bits := ipNet.Mask.Size()
	numHosts := 1 << uint(bits-ones)

	// Sanity check: prevent expanding huge subnets
	// /24 IPv4 = 256 IPs, /64 IPv6 = 18 quintillion IPs (too large!)
	// Let's set reasonable limits:
	// - IPv4: max /22 (1024 IPs)
	// - IPv6: max /120 (256 IPs)
	if bits == 32 { // IPv4
		if ones < 22 {
			return nil, fmt.Errorf("IPv4 subnet too large (/%d), maximum allowed is /22 (1024 IPs)", ones)
		}
	} else { // IPv6
		if ones < 120 {
			return nil, fmt.Errorf("IPv6 subnet too large (/%d), maximum allowed is /120 (256 IPs)", ones)
		}
	}

	// Also enforce absolute maximum
	if numHosts > 2048 {
		return nil, fmt.Errorf("subnet too large (%d IPs), maximum 2048 allowed", numHosts)
	}

	// Expand all IPs in the subnet
	for i := 0; i < numHosts; i++ {
		// Create copy of IP to avoid mutation
		nextIP := make(net.IP, len(ip))
		copy(nextIP, ip)

		// Increment IP address properly with carry
		carry := uint32(i)
		for j := len(nextIP) - 1; j >= 0 && carry > 0; j-- {
			val := uint32(nextIP[j]) + (carry & 0xFF)
			nextIP[j] = byte(val)
			carry = (carry >> 8) + (val >> 8)
		}

		// Skip network address (first) and broadcast address (last) for IPv4
		// unless it's a /31 or /32 subnet which don't have them in the traditional sense
		if bits == 32 && ones < 31 {
			if i == 0 || i == numHosts-1 {
				continue
			}
		}

		// Skip all-zeros IPv6 address (::) - it's unspecified/any address
		// Cannot be bound as a source IP
		if bits == 128 && i == 0 {
			continue
		}

		ips = append(ips, nextIP.String())
	}

	return ips, nil
}

// isIPv4 checks if an IP string is an IPv4 address.
func isIPv4(ipStr string) bool {
	ip := net.ParseIP(ipStr)
	if ip == nil {
		return false
	}
	// Check if it's IPv4 by seeing if To4() succeeds
	return ip.To4() != nil
}
