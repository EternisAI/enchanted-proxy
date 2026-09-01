package routing

import (
	"testing"
	"time"

	"github.com/eternisai/enchanted-proxy/internal/config"
)

// The probe timeout is only overridable per endpoint if the resolved config
// carries the configured value through to the probe service.
func TestProbeConfigCarriesTimeout(t *testing.T) {
	if got, want := defaultProbeConfig().Timeout, config.DefaultProbeTimeout; got != want {
		t.Errorf("default probe config: got timeout %v, want %v", got, want)
	}

	cfg := &config.ProbeConfig{Timeout: 2 * time.Minute, RetryInterval: 3 * time.Minute}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}

	resolved := probeConfigFromConfig(cfg)
	if got, want := resolved.Timeout, 2*time.Minute; got != want {
		t.Errorf("resolved probe config: got timeout %v, want %v", got, want)
	}
}
