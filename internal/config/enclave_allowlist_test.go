package config

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/goccy/go-yaml"
)

// Every model provider in config/config.yaml must be reachable from the
// enclave: its host in deploy/enclaver.yaml's egress allowlist and its API key
// variable in the env allowlist, and so must the host of every base URL a
// model's provider entry overrides. Otherwise the egress filter blocks the host
// and the key reads as empty inside the enclave, although both work outside it.
//
// A loopback base URL is an Envoy listener in the enclave (deploy/envoy), which
// forwards to its upstream through Odyn itself, so it must name a port Envoy
// listens on, and every fixed upstream Envoy tunnels to must be in the egress
// allowlist. Probe base URLs are not checked: the prober runs outside the
// enclave.
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

	envoyPorts, tunnelHosts := readEnvoyConfig(t, "../../deploy/envoy/envoy.yaml")

	if cfg.ModelRouterConfig == nil || len(cfg.ModelRouterConfig.Providers) == 0 {
		t.Fatal("no model providers in config.yaml")
	}
	checkHost := func(owner, baseURL string) {
		t.Helper()
		u, err := url.Parse(baseURL)
		if err != nil {
			t.Fatalf("%s: %v", owner, err)
		}
		if host := u.Hostname(); host == "127.0.0.1" || host == "localhost" {
			if !slices.Contains(envoyPorts, u.Port()) {
				t.Errorf("%s: %s is not a listener in the enclave's Envoy config", owner, u.Host)
			}
			return
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

	for _, hostport := range tunnelHosts {
		// A format string such as %DOWNSTREAM_DIRECT_REMOTE_ADDRESS_WITHOUT_PORT%
		// is the connection's original destination, decided per connection.
		if strings.HasPrefix(hostport, "%") {
			continue
		}
		host, _, err := net.SplitHostPort(hostport)
		if err != nil {
			t.Fatalf("Envoy tunnel %q: %v", hostport, err)
		}
		if !slices.Contains(enclave.Egress.Allow, host) {
			t.Errorf("Envoy tunnel %s: host %s is not in the enclave's egress allowlist", hostport, host)
		}
	}
}

// readEnvoyConfig returns the ports of Envoy's socket listeners and the
// hostnames its TCP proxies tunnel to through Odyn.
func readEnvoyConfig(t *testing.T, path string) (ports, tunnelHosts []string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var envoy struct {
		StaticResources struct {
			Listeners []struct {
				Address struct {
					SocketAddress struct {
						PortValue int `yaml:"port_value"`
					} `yaml:"socket_address"`
				} `yaml:"address"`
			} `yaml:"listeners"`
		} `yaml:"static_resources"`
	}
	if err := yaml.Unmarshal(raw, &envoy); err != nil {
		t.Fatal(err)
	}
	for _, l := range envoy.StaticResources.Listeners {
		if port := l.Address.SocketAddress.PortValue; port != 0 {
			ports = append(ports, fmt.Sprint(port))
		}
	}

	var doc any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	var walk func(any)
	walk = func(v any) {
		switch v := v.(type) {
		case map[string]any:
			if tc, ok := v["tunneling_config"].(map[string]any); ok {
				if h, ok := tc["hostname"].(string); ok {
					tunnelHosts = append(tunnelHosts, h)
				}
			}
			for _, child := range v {
				walk(child)
			}
		case []any:
			for _, child := range v {
				walk(child)
			}
		}
	}
	walk(doc)
	if len(ports) == 0 || len(tunnelHosts) == 0 {
		t.Fatalf("found %d listener ports and %d tunnels in %s, want both", len(ports), len(tunnelHosts), path)
	}
	return ports, tunnelHosts
}
