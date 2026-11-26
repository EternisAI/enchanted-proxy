package config

import (
	"strings"
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

// A probe base URL and mutual TLS settings parse from YAML, and settings that
// could never connect are rejected when the config loads rather than at the
// first probe.
func TestProbeConfigBaseURLAndTLS(t *testing.T) {
	const pin = "d7afxr+KiPEsZL9Vss21E6HKskR7G26PmZNZcWhBgXk="
	tests := []struct {
		name    string
		probe   string
		wantErr string
	}{
		{name: "base URL alone", probe: `
  base_url: http://upstream.example:8000/v1`},
		{name: "base URL and TLS", probe: `
  base_url: https://upstream.example:8443/v1
  tls:
    client_cert_file: /certs/tls.crt
    client_key_file: /certs/tls.key
    server_spki_sha256: [` + pin + `]`},
		{name: "relative base URL", probe: `
  base_url: /v1`, wantErr: "absolute http or https URL"},
		{name: "TLS without a base URL", probe: `
  tls:
    client_cert_file: /certs/tls.crt
    client_key_file: /certs/tls.key
    server_spki_sha256: [` + pin + `]`, wantErr: "requires an https base_url"},
		{name: "TLS over http", probe: `
  base_url: http://upstream.example:8443/v1
  tls:
    client_cert_file: /certs/tls.crt
    client_key_file: /certs/tls.key
    server_spki_sha256: [` + pin + `]`, wantErr: "requires an https base_url"},
		{name: "TLS without a key", probe: `
  base_url: https://upstream.example:8443/v1
  tls:
    client_cert_file: /certs/tls.crt
    server_spki_sha256: [` + pin + `]`, wantErr: "client_cert_file and client_key_file"},
		{name: "TLS without a pin", probe: `
  base_url: https://upstream.example:8443/v1
  tls:
    client_cert_file: /certs/tls.crt
    client_key_file: /certs/tls.key`, wantErr: "at least one server_spki_sha256"},
		{name: "pin that is not a SHA-256", probe: `
  base_url: https://upstream.example:8443/v1
  tls:
    client_cert_file: /certs/tls.crt
    client_key_file: /certs/tls.key
    server_spki_sha256: [c2hvcnQ=]`, wantErr: "is not a base64 SHA-256"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var provider ModelEndpointProvider
			err := yaml.Unmarshal([]byte("name: Eternis\nprobe:"+tt.probe+"\n"), &provider)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("got error %v, want one containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}
			if provider.Probe == nil || provider.Probe.BaseURL == "" {
				t.Fatalf("probe base URL not parsed: %+v", provider.Probe)
			}
			if strings.Contains(tt.probe, "tls:") {
				if tls := provider.Probe.TLS; tls == nil || tls.ClientCertFile != "/certs/tls.crt" ||
					tls.ClientKeyFile != "/certs/tls.key" || len(tls.ServerSPKISHA256) != 1 || tls.ServerSPKISHA256[0] != pin {
					t.Fatalf("probe TLS not parsed: %+v", provider.Probe.TLS)
				}
			}
		})
	}
}
