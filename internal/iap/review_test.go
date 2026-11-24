package iap

import (
	"testing"
	"time"
)

func TestLoadReviewConfig(t *testing.T) {
	t.Setenv("ENTITLEMENT_REVIEW_MODE", "")
	t.Setenv("ENTITLEMENT_REVIEW_CUTOFF", "")
	cfg, err := LoadReviewConfig()
	if err != nil || cfg.Mode != ReviewOff || !cfg.Cutoff.Equal(time.Date(2026, 2, 16, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("defaults %+v, %v; want off, cutoff at the end of Feb 15", cfg, err)
	}

	t.Setenv("ENTITLEMENT_REVIEW_MODE", "enforce")
	t.Setenv("ENTITLEMENT_REVIEW_CUTOFF", "2026-03-01")
	if cfg, err := LoadReviewConfig(); err != nil || cfg.Mode != ReviewEnforce || !cfg.Cutoff.Equal(time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("%+v, %v", cfg, err)
	}

	t.Setenv("ENTITLEMENT_REVIEW_MODE", "enforced")
	if cfg, err := LoadReviewConfig(); err == nil || cfg.Mode != ReviewOff {
		t.Fatalf("unknown mode: %+v, %v; want off and an error", cfg, err)
	}

	t.Setenv("ENTITLEMENT_REVIEW_MODE", "enforce")
	t.Setenv("ENTITLEMENT_REVIEW_CUTOFF", "15/02/2026")
	if cfg, err := LoadReviewConfig(); err == nil || cfg.Mode != ReviewOff {
		t.Fatalf("bad cutoff: %+v, %v; want off and an error", cfg, err)
	}
}
