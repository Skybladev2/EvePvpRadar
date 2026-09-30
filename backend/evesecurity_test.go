package main

import (
	"math"
	"testing"
)

func TestDisplayEveSecurityForUI(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   float64
		want float64
	}{
		{0, 0},
		{-0.1, 0},
		{math.Copysign(0, -1), 0},
		{0.035519, 0.1}, // Feshur (SDE) — 0 < truesec < 0.1
		{0.049, 0.1},
		{0.05, 0.1},
		{0.099, 0.1},
		{0.1, 0.1},
		{0.15, 0.2},
		{0.895912, 0.9},
	}
	for _, tc := range cases {
		if got := displayEveSecurityForUI(tc.in); got != tc.want {
			t.Errorf("displayEveSecurityForUI(%v) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// TestSecurityFilterBand covers the checkbox that controls a row. Lawless systems are escalated one
// band down (highsec -> lowsec, lowsec -> nullsec); nullsec stays nullsec. Display value is separate.
func TestSecurityFilterBand(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name         string
		displayValue float64
		lawless      bool
		want         string
	}{
		{"nullsec", -0.5, false, "nullsec"},
		{"nullsec stays nullsec (lawless by design)", -0.5, true, "nullsec"},
		{"lowsec", 0.3, false, "lowsec"},
		{"lowsec lawless becomes nullsec", 0.3, true, "nullsec"},
		{"highsec", 0.5, false, "highsec"},
		{"highsec lawless becomes lowsec", 0.5, true, "lowsec"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := securityFilterBand(tc.displayValue, tc.lawless); got != tc.want {
				t.Errorf("securityFilterBand(%v, %v) = %q, want %q", tc.displayValue, tc.lawless, got, tc.want)
			}
		})
	}
}
