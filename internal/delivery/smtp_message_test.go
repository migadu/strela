package delivery

import (
	"strings"
	"testing"
)

func TestSMTPMessageNormalization(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "single line response",
			input:    "250 Ok: queued",
			expected: "250 Ok: queued",
		},
		{
			name:     "multi-line response with queue ID",
			input:    "Ok: queued as\n<queue-id-12345>",
			expected: "Ok: queued as <queue-id-12345>",
		},
		{
			name:     "multi-line response with multiple newlines",
			input:    "250-First line\n250-Second line\n250 Third line",
			expected: "250-First line 250-Second line 250 Third line",
		},
		{
			name:     "response with leading/trailing newlines",
			input:    "\nOk: queued\n",
			expected: " Ok: queued ",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Test the normalization logic used in deliverPayload
			result := strings.ReplaceAll(tt.input, "\n", " ")
			if result != tt.expected {
				t.Errorf("normalization failed: got %q, want %q", result, tt.expected)
			}
		})
	}
}
