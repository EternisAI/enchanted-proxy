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

// Probing an endpoint behind mutual TLS needs its probe base URL and TLS
// settings to reach the probe service unchanged.
func TestProbeConfigCarriesBaseURLAndTLS(t *testing.T) {
	tlsCfg := &config.ProbeTLSConfig{
		ClientCertFile:   "/certs/tls.crt",
		ClientKeyFile:    "/certs/tls.key",
		ServerSPKISHA256: []string{"d7afxr+KiPEsZL9Vss21E6HKskR7G26PmZNZcWhBgXk="},
	}
	cfg := &config.ProbeConfig{BaseURL: "https://upstream.example:8443/v1", TLS: tlsCfg}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}

	resolved := probeConfigFromConfig(cfg)
	if resolved.BaseURL != cfg.BaseURL {
		t.Errorf("resolved probe config: got base URL %q, want %q", resolved.BaseURL, cfg.BaseURL)
	}
	if resolved.TLS != tlsCfg {
		t.Errorf("resolved probe config: got TLS %+v, want %+v", resolved.TLS, tlsCfg)
	}
}
