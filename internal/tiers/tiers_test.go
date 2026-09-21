package tiers

import "testing"

func TestMeetsMinTier(t *testing.T) {
	cases := []struct {
		tier    Tier
		minTier string
		want    bool
	}{
		{TierFree, "", true},
		{TierPlus, "", true},
		{TierPro, "", true},

		{TierFree, "pro", false},
		{TierPlus, "pro", false},
		{TierPro, "pro", true},

		{TierFree, "plus", false},
		{TierPlus, "plus", true},
		{TierPro, "plus", true},

		// An unrecognized floor denies rather than opens the model up.
		{TierPro, "enterprise", false},
	}

	for _, tc := range cases {
		// Configs rather than Get: Get reads the global config.AppConfig, which no test
		// populates, and the soft multiplier it applies is irrelevant here.
		cfg, exists := Configs[tc.tier]
		if !exists {
			t.Fatalf("no config for tier %q", tc.tier)
		}
		if got := cfg.MeetsMinTier(tc.minTier); got != tc.want {
			t.Errorf("%s.MeetsMinTier(%q) = %v, want %v", tc.tier, tc.minTier, got, tc.want)
		}
	}
}

// A tier config that never came from Configs must not satisfy a floor.
func TestMeetsMinTierUnknownTier(t *testing.T) {
	cfg := Config{Name: "legacy"}
	if cfg.MeetsMinTier("plus") {
		t.Error("unknown tier satisfied a plus floor")
	}
	if !cfg.MeetsMinTier("") {
		t.Error("unknown tier should still pass when no floor is set")
	}
}

func TestDisplayNameFor(t *testing.T) {
	if got := DisplayNameFor("pro"); got != "Pro" {
		t.Errorf("DisplayNameFor(pro) = %q, want Pro", got)
	}
	if got := DisplayNameFor("enterprise"); got != "enterprise" {
		t.Errorf("DisplayNameFor(enterprise) = %q, want the raw value back", got)
	}
}
