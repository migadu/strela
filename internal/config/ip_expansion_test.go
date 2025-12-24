package config

import (
	"testing"
)

func TestExpandSourceIPs_IndividualIPs(t *testing.T) {
	tests := []struct {
		name        string
		sourceIPs   []string
		wantV4      []string
		wantV6      []string
		wantErr     bool
		errContains string
	}{
		{
			name:      "single IPv4",
			sourceIPs: []string{"192.0.2.1"},
			wantV4:    []string{"192.0.2.1"},
			wantV6:    []string{},
			wantErr:   false,
		},
		{
			name:      "single IPv6",
			sourceIPs: []string{"2001:db8::1"},
			wantV4:    []string{},
			wantV6:    []string{"2001:db8::1"},
			wantErr:   false,
		},
		{
			name:      "mixed IPv4 and IPv6",
			sourceIPs: []string{"192.0.2.1", "2001:db8::1", "192.0.2.2"},
			wantV4:    []string{"192.0.2.1", "192.0.2.2"},
			wantV6:    []string{"2001:db8::1"},
			wantErr:   false,
		},
		{
			name:        "invalid IP",
			sourceIPs:   []string{"not-an-ip"},
			wantErr:     true,
			errContains: "invalid IP address",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := ExpandSourceIPs(tt.sourceIPs)
			if tt.wantErr {
				if err == nil {
					t.Errorf("ExpandSourceIPs() expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Errorf("ExpandSourceIPs() unexpected error: %v", err)
				return
			}

			if len(result.IPv4) != len(tt.wantV4) {
				t.Errorf("ExpandSourceIPs() IPv4 count = %d, want %d", len(result.IPv4), len(tt.wantV4))
			}
			if len(result.IPv6) != len(tt.wantV6) {
				t.Errorf("ExpandSourceIPs() IPv6 count = %d, want %d", len(result.IPv6), len(tt.wantV6))
			}

			// Check IPv4 values
			for i, ip := range result.IPv4 {
				if i >= len(tt.wantV4) {
					break
				}
				if ip != tt.wantV4[i] {
					t.Errorf("ExpandSourceIPs() IPv4[%d] = %s, want %s", i, ip, tt.wantV4[i])
				}
			}

			// Check IPv6 values
			for i, ip := range result.IPv6 {
				if i >= len(tt.wantV6) {
					break
				}
				if ip != tt.wantV6[i] {
					t.Errorf("ExpandSourceIPs() IPv6[%d] = %s, want %s", i, ip, tt.wantV6[i])
				}
			}
		})
	}
}

func TestExpandSourceIPs_CIDRSubnets(t *testing.T) {
	tests := []struct {
		name        string
		sourceIPs   []string
		wantV4Count int
		wantV6Count int
		wantErr     bool
		errContains string
	}{
		{
			name:        "IPv4 /30 subnet",
			sourceIPs:   []string{"192.0.2.0/30"},
			wantV4Count: 2, // .1 and .2 (skip network and broadcast)
			wantV6Count: 0,
			wantErr:     false,
		},
		{
			name:        "IPv4 /28 subnet",
			sourceIPs:   []string{"192.0.2.0/28"},
			wantV4Count: 14, // 16 total - 2 (network/broadcast)
			wantV6Count: 0,
			wantErr:     false,
		},
		{
			name:        "IPv6 /126 subnet",
			sourceIPs:   []string{"2001:db8::/126"},
			wantV4Count: 0,
			wantV6Count: 4, // All 4 IPs (no network/broadcast in IPv6)
			wantErr:     false,
		},
		{
			name:        "IPv4 subnet too large",
			sourceIPs:   []string{"192.0.2.0/20"},
			wantErr:     true,
			errContains: "too large",
		},
		{
			name:        "IPv6 subnet too large",
			sourceIPs:   []string{"2001:db8::/112"},
			wantErr:     true,
			errContains: "too large",
		},
		{
			name:        "mixed individual and CIDR",
			sourceIPs:   []string{"192.0.2.1", "192.0.2.0/30", "2001:db8::1"},
			wantV4Count: 3, // 1 individual + 2 from /30
			wantV6Count: 1,
			wantErr:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := ExpandSourceIPs(tt.sourceIPs)
			if tt.wantErr {
				if err == nil {
					t.Errorf("ExpandSourceIPs() expected error containing %q, got nil", tt.errContains)
				}
				return
			}
			if err != nil {
				t.Errorf("ExpandSourceIPs() unexpected error: %v", err)
				return
			}

			if len(result.IPv4) != tt.wantV4Count {
				t.Errorf("ExpandSourceIPs() IPv4 count = %d, want %d", len(result.IPv4), tt.wantV4Count)
			}
			if len(result.IPv6) != tt.wantV6Count {
				t.Errorf("ExpandSourceIPs() IPv6 count = %d, want %d", len(result.IPv6), tt.wantV6Count)
			}
		})
	}
}

func TestExpandSourceIPs_EmptyInput(t *testing.T) {
	result, err := ExpandSourceIPs([]string{})
	if err != nil {
		t.Errorf("ExpandSourceIPs() with empty input returned error: %v", err)
	}
	if len(result.IPv4) != 0 {
		t.Errorf("ExpandSourceIPs() IPv4 count = %d, want 0", len(result.IPv4))
	}
	if len(result.IPv6) != 0 {
		t.Errorf("ExpandSourceIPs() IPv6 count = %d, want 0", len(result.IPv6))
	}
}

func TestIsIPv4(t *testing.T) {
	tests := []struct {
		ip   string
		want bool
	}{
		{"192.0.2.1", true},
		{"10.0.0.1", true},
		{"2001:db8::1", false},
		{"::1", false},
		{"invalid", false},
	}

	for _, tt := range tests {
		t.Run(tt.ip, func(t *testing.T) {
			got := isIPv4(tt.ip)
			if got != tt.want {
				t.Errorf("isIPv4(%q) = %v, want %v", tt.ip, got, tt.want)
			}
		})
	}
}
