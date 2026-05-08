package handler

import (
	"strings"
	"testing"
)

func TestValidateEmailAddress(t *testing.T) {
	tests := []struct {
		name      string
		addr      string
		fieldName string
		allowNull bool
		wantErr   bool
		errMsg    string // substring expected in error message
	}{
		// Valid addresses
		{name: "simple address", addr: "user@example.com", fieldName: "to", wantErr: false},
		{name: "subdomain address", addr: "user@mail.example.com", fieldName: "to", wantErr: false},
		{name: "plus addressing", addr: "user+tag@example.com", fieldName: "to", wantErr: false},
		{name: "dotted local part", addr: "first.last@example.com", fieldName: "to", wantErr: false},
		{name: "numeric local part", addr: "123@example.com", fieldName: "to", wantErr: false},
		{name: "hyphenated domain", addr: "user@my-domain.com", fieldName: "to", wantErr: false},
		{name: "angle brackets", addr: "<user@example.com>", fieldName: "to", wantErr: false},
		{name: "long but valid", addr: "a@example.com", fieldName: "to", wantErr: false},
		{name: "IDN domain (non-ASCII)", addr: "user@münchen.de", fieldName: "to", wantErr: false},

		// Null sender (from only)
		{name: "null sender empty", addr: "", fieldName: "from", allowNull: true, wantErr: true, errMsg: "address is empty"},
		{name: "null sender angle brackets", addr: "<>", fieldName: "from", allowNull: true, wantErr: false},
		{name: "null sender not allowed for to", addr: "<>", fieldName: "to", allowNull: false, wantErr: true, errMsg: "address is empty"},
		{name: "empty to rejected", addr: "", fieldName: "to", allowNull: false, wantErr: true, errMsg: "address is empty"},

		// Missing @
		{name: "no at sign", addr: "userexample.com", fieldName: "to", wantErr: true, errMsg: "missing '@'"},
		{name: "only local part", addr: "user", fieldName: "to", wantErr: true, errMsg: "missing '@'"},

		// Empty parts
		{name: "empty local part", addr: "@example.com", fieldName: "to", wantErr: true, errMsg: "empty local part"},
		{name: "empty domain", addr: "user@", fieldName: "to", wantErr: true, errMsg: "empty domain"},

		// Domain validation
		{name: "no TLD (bare hostname)", addr: "user@localhost", fieldName: "to", wantErr: true, errMsg: "no TLD"},
		{name: "domain starts with dot", addr: "user@.example.com", fieldName: "to", wantErr: true, errMsg: "starts with invalid"},
		{name: "domain ends with dot", addr: "user@example.com.", fieldName: "to", wantErr: true, errMsg: "ends with invalid"},
		{name: "domain starts with hyphen", addr: "user@-example.com", fieldName: "to", wantErr: true, errMsg: "starts with invalid"},
		{name: "domain ends with hyphen", addr: "user@example.com-", fieldName: "to", wantErr: true, errMsg: "ends with invalid"},
		{name: "consecutive dots in domain", addr: "user@example..com", fieldName: "to", wantErr: true, errMsg: "consecutive dots"},
		{name: "underscore in domain", addr: "user@exam_ple.com", fieldName: "to", wantErr: true, errMsg: "invalid character"},
		{name: "space in domain", addr: "user@exam ple.com", fieldName: "to", wantErr: true, errMsg: "invalid character"},

		// Length limits
		{name: "long local part (65 chars) is allowed", addr: strings.Repeat("a", 65) + "@example.com", fieldName: "to", wantErr: false},

		// Domain label limits
		{name: "domain label too long (64 chars)", addr: "user@" + strings.Repeat("a", 64) + ".com", fieldName: "to", wantErr: true, errMsg: "exceeds 63 characters"},

		// Field name in error
		{name: "error includes field name from", addr: "bad", fieldName: "from", wantErr: true, errMsg: "from:"},
		{name: "error includes field name to", addr: "bad", fieldName: "to", wantErr: true, errMsg: "to:"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateEmailAddress(tt.addr, tt.fieldName, tt.allowNull)

			if tt.wantErr {
				if err == nil {
					t.Errorf("validateEmailAddress(%q) = nil, want error containing %q", tt.addr, tt.errMsg)
					return
				}
				if tt.errMsg != "" && !strings.Contains(err.Error(), tt.errMsg) {
					t.Errorf("validateEmailAddress(%q) error = %q, want error containing %q", tt.addr, err.Error(), tt.errMsg)
				}
			} else {
				if err != nil {
					t.Errorf("validateEmailAddress(%q) = %v, want nil", tt.addr, err)
				}
			}
		})
	}
}

func TestValidateEmailAddress_TotalLengthLimit(t *testing.T) {
	// Build an address that exceeds 254 characters total
	localPart := strings.Repeat("a", 64)
	// Need domain to push total over 254: 64 (local) + 1 (@) + 190+ (domain) = 255+
	domain := strings.Repeat("a", 63) + "." + strings.Repeat("b", 63) + "." + strings.Repeat("c", 63) + ".com"
	addr := localPart + "@" + domain

	if len(addr) <= 254 {
		t.Fatalf("Test setup error: address length %d is not > 254", len(addr))
	}

	err := validateEmailAddress(addr, "to", false)
	if err == nil {
		t.Error("Expected error for address exceeding 254 characters")
	}
	if !strings.Contains(err.Error(), "exceeds 254") {
		t.Errorf("Expected 'exceeds 254' error, got: %v", err)
	}
}

func TestValidateEmailAddress_ValidEdgeCases(t *testing.T) {
	// These are valid edge cases that should pass
	validAddresses := []string{
		"a@b.co",                                   // Minimal valid
		"user@example.co.uk",                       // Multi-level TLD
		"user@123.123.123.com",                     // Numeric domain labels
		"user@a-b.c-d.example.com",                 // Hyphenated labels
		strings.Repeat("a", 64) + "@example.com",   // Max local part length
		"user@" + strings.Repeat("a", 63) + ".com", // Max label length
	}

	for _, addr := range validAddresses {
		t.Run(addr, func(t *testing.T) {
			if err := validateEmailAddress(addr, "to", false); err != nil {
				t.Errorf("validateEmailAddress(%q) = %v, want nil", addr, err)
			}
		})
	}
}
