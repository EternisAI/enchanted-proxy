package config

import (
	"strings"
	"testing"
)

func TestTitleGenerationConfigValidate(t *testing.T) {
	router := &ModelRouterConfig{Providers: []ModelProviderConfig{
		{Name: "Tinfoil", BaseURL: "https://inference.tinfoil.sh/v1", APIKeyEnvVar: "TINFOIL_API_KEY"},
		{Name: "OpenRouter", BaseURL: "https://openrouter.ai/api/v1"},
		{Name: "Eternis", APIKeyEnvVar: "ETERNIS_INFERENCE_API_KEY"},
	}}

	cases := []struct {
		name            string
		provider, model string
		wantErr         string
	}{
		{name: "neither set"},
		{name: "both set", provider: "Tinfoil", model: "gemma4-31b"},
		{name: "model without provider", model: "gemma4-31b", wantErr: "set together"},
		{name: "provider without model", provider: "Tinfoil", wantErr: "set together"},
		{name: "unknown provider", provider: "Nowhere", model: "m", wantErr: `unknown provider "Nowhere"`},
		{name: "key chosen per request", provider: "OpenRouter", model: "m", wantErr: "api_key_env_var"},
		{name: "base URL set per model", provider: "Eternis", model: "m", wantErr: "base_url"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &TitleGenerationConfig{Provider: tc.provider, Model: tc.model}
			err := cfg.Validate(router)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("got error %v, want none", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Fatalf("got error %v, want one containing %q", err, tc.wantErr)
			}
		})
	}
}

// A title model the validation refuses stops the configuration from loading.
func TestLoadConfigFileRefusesAnUnknownTitleProvider(t *testing.T) {
	const file = `
title_generation:
  provider: Nowhere
  model: gemma4-31b
  initial_prompt: Title this.
model_router:
  providers:
  - name: Tinfoil
    api_key_env_var: TINFOIL_API_KEY
    base_url: https://inference.tinfoil.sh/v1
  models:
  - name: meta-llama/Llama-3.3-70B
    providers:
    - name: Tinfoil
`
	err := LoadConfigFile(strings.NewReader(file), &Config{})
	if err == nil || !strings.Contains(err.Error(), `unknown provider "Nowhere"`) {
		t.Fatalf("got error %v, want one about the unknown title provider", err)
	}

	fixed := strings.Replace(file, "provider: Nowhere", "provider: Tinfoil", 1)
	var cfg Config
	if err := LoadConfigFile(strings.NewReader(fixed), &cfg); err != nil {
		t.Fatalf("loading with a known title provider: %v", err)
	}
	if got := cfg.TitleGeneration; got.Provider != "Tinfoil" || got.Model != "gemma4-31b" {
		t.Fatalf("title model loaded as %q on %q, want gemma4-31b on Tinfoil", got.Model, got.Provider)
	}
}
