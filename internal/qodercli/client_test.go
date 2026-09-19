package qodercli

import "testing"

func TestClampTierStopsAtTheStrongestTierTheModelOffers(t *testing.T) {
	// Measured shapes: Qwen3.8-Flash has no high/max rung, GLM-5.3 has no
	// medium/xhigh rung. Asking for a missing tier must never return it, because
	// the CLI would silently run the turn with reasoning_effort=none.
	cases := []struct {
		name   string
		ladder []string
		in     string
		want   string
	}{
		{"qwen keeps its own tiers", []string{"none", "low", "medium", "xhigh"}, "low", "low"},
		{"qwen steps high down to medium", []string{"none", "low", "medium", "xhigh"}, "high", "medium"},
		{"qwen caps max at its top", []string{"none", "low", "medium", "xhigh"}, "max", "xhigh"},
		{"glm keeps its top tier", []string{"none", "low", "high", "max"}, "max", "max"},
		{"glm steps xhigh down to high", []string{"none", "low", "high", "max"}, "xhigh", "high"},
		{"glm steps medium down to low", []string{"none", "low", "high", "max"}, "medium", "low"},
		{"unknown tier passes through", []string{"none", "low"}, "banana", "banana"},
		{"measured tier-less model sends nothing", nil, "xhigh", ""},
		// Auto exposes no thinking tier at all: forcing none would switch its
		// default thinking off instead of picking a level.
		{"tier-less model omits instead of forcing none", []string{"none"}, "medium", ""},
		{"explicit none still reaches a tier-less model", []string{"none"}, "none", "none"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := clampTier(tc.ladder, tc.in); got != tc.want {
				t.Fatalf("clampTier(%v, %q) = %q, want %q", tc.ladder, tc.in, got, tc.want)
			}
		})
	}
}
