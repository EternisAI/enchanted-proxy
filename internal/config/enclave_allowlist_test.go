package config

import (
	"net/url"
	"os"
	"slices"
	"testing"

	"github.com/goccy/go-yaml"
)

// Every model provider in config/config.yaml must be reachable from the
// enclave: its host in deploy/enclaver.yaml's egress allowlist and its API key
// variable in the env allowlist, and so must the host of every base URL a
// model's provider entry overrides. Otherwise the egress filter blocks the host
// and the key reads as empty inside the enclave, although both work outside it.
func TestModelProvidersAreAllowedInTheEnclave(t *testing.T) {
	f, err := os.Open("../../config/config.yaml")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var cfg Config
	if err := LoadConfigFile(f, &cfg); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile("../../deploy/enclaver.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var enclave struct {
		Egress struct {
			Allow []string `yaml:"allow"`
		} `yaml:"egress"`
		Env []string `yaml:"env"`
	}
	if err := yaml.Unmarshal(raw, &enclave); err != nil {
		t.Fatal(err)
	}

	if cfg.ModelRouterConfig == nil || len(cfg.ModelRouterConfig.Providers) == 0 {
		t.Fatal("no model providers in config.yaml")
	}
	checkHost := func(owner, baseURL string) {
		t.Helper()
		u, err := url.Parse(baseURL)
		if err != nil {
			t.Fatalf("%s: %v", owner, err)
		}
		if !slices.Contains(enclave.Egress.Allow, u.Hostname()) {
			t.Errorf("%s: host %s is not in the enclave's egress allowlist", owner, u.Hostname())
		}
	}
	for _, p := range cfg.ModelRouterConfig.Providers {
		// A provider without a base URL takes it from each model's entry.
		if p.BaseURL != "" {
			checkHost(p.Name, p.BaseURL)
		}
		if p.APIKeyEnvVar != "" && !slices.Contains(enclave.Env, p.APIKeyEnvVar) {
			t.Errorf("%s: %s is not in the enclave's env allowlist", p.Name, p.APIKeyEnvVar)
		}
	}
	for _, m := range cfg.ModelRouterConfig.Models {
		for _, p := range m.Providers {
			if p.BaseURL != "" {
				checkHost(m.Name+" via "+p.Name, p.BaseURL)
			}
		}
	}
}
