package config

import (
	"testing"
	"time"

	"github.com/goccy/go-yaml"
)

func TestProbeConfigTimeoutDefaultsAndClamping(t *testing.T) {
	tests := []struct {
		name     string
		interval time.Duration
		timeout  time.Duration
		want     time.Duration
	}{
		{
			name: "omitted takes the default",
			want: DefaultProbeTimeout,
		},
		{
			name:    "explicit value is kept",
			timeout: 2 * time.Minute,
			want:    2 * time.Minute,
		},
		{
			name:    "below the minimum is raised",
			timeout: time.Millisecond,
			want:    MinProbeTimeout,
		},
		{
			name:    "above the maximum is capped",
			timeout: time.Hour,
			want:    MaxProbeTimeout,
		},
		{
			name:     "cannot outlive its own interval",
			interval: time.Minute,
			timeout:  5 * time.Minute,
			want:     time.Minute,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &ProbeConfig{Interval: tt.interval, Timeout: tt.timeout}
			if err := cfg.Validate(); err != nil {
				t.Fatalf("Validate: %v", err)
			}
			if cfg.Timeout != tt.want {
				t.Errorf("got timeout %v, want %v", cfg.Timeout, tt.want)
			}
		})
	}
}

// The per-endpoint override is only useful if a YAML duration reaches it intact.
func TestProbeConfigTimeoutFromYAML(t *testing.T) {
	var provider ModelEndpointProvider
	input := []byte(`
name: NEAR AI
model: z-ai/glm-5.2
probe:
  timeout: 2m
  retry_interval: 3m
`)
	if err := yaml.Unmarshal(input, &provider); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if provider.Probe == nil {
		t.Fatal("probe config not parsed")
	}
	if got, want := provider.Probe.Timeout, 2*time.Minute; got != want {
		t.Errorf("got timeout %v, want %v", got, want)
	}
	if got, want := provider.Probe.RetryInterval, 3*time.Minute; got != want {
		t.Errorf("got retry interval %v, want %v", got, want)
	}
}
