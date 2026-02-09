package delivery

import (
	"testing"
)

func TestNeedsUTF8Address(t *testing.T) {
	tests := []struct {
		name     string
		email    string
		expected bool
	}{
		{
			name:     "ASCII only email",
			email:    "user@example.com",
			expected: false,
		},
		{
			name:     "UTF-8 in local part",
			email:    "用户@example.com",
			expected: true,
		},
		{
			name:     "UTF-8 in domain",
			email:    "user@例え.jp",
			expected: true,
		},
		{
			name:     "UTF-8 in both parts",
			email:    "用户@例え.jp",
			expected: true,
		},
		{
			name:     "Accented characters",
			email:    "café@example.com",
			expected: true,
		},
		{
			name:     "Cyrillic characters",
			email:    "пользователь@пример.рф",
			expected: true,
		},
		{
			name:     "Empty string",
			email:    "",
			expected: false,
		},
		{
			name:     "Special ASCII characters",
			email:    "user+tag@example.com",
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := needsUTF8Address(tt.email)
			if result != tt.expected {
				t.Errorf("needsUTF8Address(%q) = %v, want %v", tt.email, result, tt.expected)
			}
		})
	}
}
