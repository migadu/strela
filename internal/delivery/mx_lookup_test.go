package delivery

import (
	"errors"
	"fmt"
	"net"
	"testing"
)

// isDNSNotFound drives the permanent/transient split in lookupDNS: a "not found"
// result (NXDOMAIN/NODATA) is permanent, anything else (SERVFAIL, timeout,
// network) is transient and must stay retryable. Lock that classification.
func TestIsDNSNotFound(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nxdomain/nodata", &net.DNSError{Err: "no such host", IsNotFound: true}, true},
		{"wrapped not found", fmt.Errorf("MX lookup failed: %w", &net.DNSError{IsNotFound: true}), true},
		{"timeout", &net.DNSError{Err: "i/o timeout", IsTimeout: true}, false},
		{"temporary servfail", &net.DNSError{Err: "server misbehaving", IsTemporary: true}, false},
		{"dial failure (not a DNSError)", errors.New("all DNS resolvers failed"), false},
		{"wrapped transient", fmt.Errorf("address lookup failed: %w", &net.DNSError{IsTimeout: true}), false},
		{"nil", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isDNSNotFound(tc.err); got != tc.want {
				t.Fatalf("isDNSNotFound(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
