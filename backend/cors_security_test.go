package main

import "testing"

func TestNormalizeOriginKey(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"http://localhost:8888/", "http://localhost:8888"},
		{"HTTPS://Example.COM/path", "https://example.com"},
		{"", ""},
		{"not-a-url", ""},
	}
	for _, tc := range tests {
		if got := normalizeOriginKey(tc.in); got != tc.want {
			t.Errorf("normalizeOriginKey(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
