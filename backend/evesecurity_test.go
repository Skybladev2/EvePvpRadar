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
