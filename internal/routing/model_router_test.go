package routing

import (
	"fmt"
	"log/slog"
	"sort"
	"testing"

	"github.com/eternisai/enchanted-proxy/internal/config"
	"github.com/eternisai/enchanted-proxy/internal/logger"
)

var (
	EternisAPIKey           = "test-eternis-key"
	NearAIAPIKey            = "test-near-ai-key"
	OpenAIAPIKey            = "test-openai-key"
	OpenRouterDesktopAPIKey = "test-openrouter-desktop-key"
	OpenRouterMobileAPIKey  = "test-openrouter-mobile-key"
	TinfoilAPIKey           = "test-tinfoil-key"

	Env = map[string]string{
		"CONFIG_FILE":                "testdata/config.yaml",
		"ETERNIS_INFERENCE_API_KEY":  EternisAPIKey,
		"NEAR_AI_KEY":                NearAIAPIKey,
		"OPENAI_API_KEY":             OpenAIAPIKey,
		"OPENROUTER_DESKTOP_API_KEY": OpenRouterDesktopAPIKey,
		"OPENROUTER_MOBILE_API_KEY":  OpenRouterMobileAPIKey,
		"TINFOIL_API_KEY":            TinfoilAPIKey,
	}

	router *ModelRouter
	log    *logger.Logger
)

func TestMain(t *testing.T) {
	for name, value := range Env {
		t.Setenv(name, value)
	}

	config.LoadConfig()

	if config.AppConfig.OpenRouterMobileAPIKey != OpenRouterMobileAPIKey ||
		config.AppConfig.OpenRouterDesktopAPIKey != OpenRouterDesktopAPIKey {
		t.Error("app config did not initialize correctly")
	}

	if testing.Verbose() {
		log = logger.New(logger.Config{Level: slog.LevelDebug})
	} else {
		log = logger.New(logger.Config{Level: slog.LevelError})
	}

	router = NewModelRouter(config.AppConfig, log)

	if router == nil {
		t.Fatal("NewModelRouter returned nil")
	}

	if len(router.routes) == 0 {
		t.Fatal("routes map is empty")
	}
}

func TestRouteModelExactMatch(t *testing.T) {
	tests := []struct {
		model            string
		expectedBaseURL  string
		expectedKey      string
		expectedAPIType  config.APIType
		expectedProvider string
	}{
		// To ensure the "exact" match, use the "canonical" model names and the models that
		// don't have model name overrides on the endpoint configuration level in the testdata.
		{"zai-org/GLM-4.6", "http://127.0.0.1:20001/v1", EternisAPIKey, config.APITypeChatCompletions, "Eternis"},
		{"dphn/Dolphin-Mistral-24B-Venice-Edition", "http://34.30.193.13:8000/v1", EternisAPIKey, config.APITypeChatCompletions, "Eternis"},
		{"openai/gpt-4.1", "https://openrouter.ai/api/v1", OpenRouterMobileAPIKey, config.APITypeChatCompletions, "OpenRouter"},
		{"openai/gpt-5", "https://openrouter.ai/api/v1", OpenRouterMobileAPIKey, config.APITypeChatCompletions, "OpenRouter"},
	}

	for _, tt := range tests {
		t.Run(tt.model, func(t *testing.T) {
			provider, err := router.RouteModel(tt.model, "mobile")
			if err != nil {
				t.Fatalf("RouteModel failed: %v", err)
			}
			if provider.Model != tt.model {
				t.Errorf("expected model %s, got %s", tt.model, provider.Model)
			}
			if provider.BaseURL != tt.expectedBaseURL {
				t.Errorf("expected baseURL %s, got %s", tt.expectedBaseURL, provider.BaseURL)
			}
			if provider.APIKey != tt.expectedKey {
				t.Errorf("expected API key %s, got %s", tt.expectedKey, provider.APIKey)
			}
			if provider.Name != tt.expectedProvider {
				t.Errorf("expected provider name '%s', got %s", tt.expectedProvider, provider.Name)
			}
			if provider.APIType != tt.expectedAPIType {
				t.Errorf("expected APIType %s, got %s", tt.expectedAPIType, provider.APIType)
			}
		})
	}
}

func TestRouteModelAliasMatch(t *testing.T) {
	// Map from supported aliases to the names expected by the configured provider
	tests := map[string]string{
		"deepseek/deepseek-r1-0528": "deepseek-r1-0528",
		"llama-3.3-70b":             "llama3-3-70b",
		"z-ai/glm-4.6":              "zai-org/GLM-4.6",
		"dolphin-mistral-eternis":   "dphn/Dolphin-Mistral-24B-Venice-Edition",
		"gpt-4.1":                   "openai/gpt-4.1",
		"gpt-5":                     "openai/gpt-5",
		"openai/gpt-5-pro":          "gpt-5-pro",
		"openai/gpt-4":              "gpt-4",
		"openai/gpt-4-turbo":        "gpt-4-turbo",
		"openai/gpt-3.5-turbo":      "gpt-3.5-turbo",
		"openai/o1-preview":         "o1-preview",
		"openai/o1-mini":            "o1-mini",
		"openai/o3-mini":            "o3-mini",
	}

	for alias, model := range tests {
		t.Run(alias, func(t *testing.T) {
			provider, err := router.RouteModel(alias, "mobile")
			if err != nil {
				t.Fatalf("RouteModel failed for %s: %v", alias, err)
			}
			if provider.Model != model {
				t.Errorf("expected alias %s to resolve to %s, got %s", alias, model, provider.Model)
			}
		})
	}
}

func TestRouteModelPrefixMatch(t *testing.T) {
	tests := []string{
		"gpt-4-0125-preview",
		"gpt-4-turbo-preview",
		"gpt-4-vision-preview",
		"gpt-3.5-turbo-16k",
		"o1-preview-2024",
	}

	for _, model := range tests {
		t.Run(model, func(t *testing.T) {
			provider, err := router.RouteModel(model, "mobile")
			if err != nil {
				t.Fatalf("RouteModel failed for %s: %v", model, err)
			}
			if provider.Name != "OpenAI" {
				t.Errorf("expected OpenAI for %s, got %s", model, provider.Name)
			}
		})
	}
}

func TestRouteModelFallbackToOpenRouter(t *testing.T) {
	unknownModels := []string{
		"claude-3-opus-20240229",
		"llama-2-70b-chat",
		"mistral-large",
		"gemini-pro",
	}

	for _, model := range unknownModels {
		t.Run(model, func(t *testing.T) {
			provider, err := router.RouteModel(model, "mobile")
			if err != nil {
				t.Fatalf("RouteModel failed for unknown model %s: %v", model, err)
			}
			if provider.Name != "OpenRouter" {
				t.Errorf("expected OpenRouter fallback for %s, got %s", model, provider.Name)
			}
			if provider.BaseURL != "https://openrouter.ai/api/v1" {
				t.Errorf("expected OpenRouter baseURL, got %s", provider.BaseURL)
			}
			if provider.APIKey != OpenRouterMobileAPIKey {
				t.Errorf("expected mobile key, got %s", provider.APIKey)
			}
		})
	}
}

func TestRouteModelPlatformSpecificKeys(t *testing.T) {
	tests := []struct {
		platform    string
		expectedKey string
	}{
		{"mobile", OpenRouterMobileAPIKey},
		{"desktop", OpenRouterDesktopAPIKey},
		{"unknown", OpenRouterMobileAPIKey}, // Falls back to mobile
		{"", OpenRouterMobileAPIKey},        // Empty defaults to mobile
	}

	for _, tt := range tests {
		t.Run(tt.platform, func(t *testing.T) {
			provider, err := router.RouteModel("unknown-model", tt.platform)
			if err != nil {
				t.Fatalf("RouteModel failed: %v", err)
			}
			if provider.APIKey != tt.expectedKey {
				t.Errorf("expected API key %s for platform %s, got %s", tt.expectedKey, tt.platform, provider.APIKey)
			}
		})
	}
}

func TestRouteModelEmptyModel(t *testing.T) {
	_, err := router.RouteModel("", "mobile")
	if err == nil {
		t.Error("expected error for empty model ID")
	}
}

func TestRouteModelNoProviderConfigured(t *testing.T) {
	// Save the pre-initialized global configuration as the sub-tests will modify it
	// NOTE: in the current implementation, this test cannot be run in parallel to other
	// tests that use config.AppConfig to initialize new ModelRouters or use t.Setenv()
	appConfig := config.AppConfig

	// Regenerate the configuration for the test with no provider API keys set
	t.Setenv("CONFIG_FILE", Env["CONFIG_FILE"])

	config.LoadConfig()
	router := NewModelRouter(config.AppConfig, log)

	provider, err := router.RouteModel("gpt-4", "mobile")
	if err == nil {
		t.Errorf("expected error when no provider keys are configured, got %v", provider.Name)
		router.logger.Info(fmt.Sprintf("%#v", router.routes))
	}

	// Restore the pre-initialized global configuration
	config.AppConfig = appConfig
}

func TestRouteModelCaseInsensitive(t *testing.T) {
	tests := []string{
		"GPT-4",
		"Gpt-4",
		"gPt-4",
		"GPT-4-TURBO",
	}

	for _, model := range tests {
		t.Run(model, func(t *testing.T) {
			provider, err := router.RouteModel(model, "mobile")
			if err != nil {
				t.Fatalf("RouteModel failed for %s: %v", model, err)
			}
			if provider.Name != "OpenAI" {
				t.Errorf("expected OpenAI for case-insensitive match, got %s", provider.Name)
			}
		})
	}
}

func TestGetSupportedModels(t *testing.T) {
	models := router.GetSupportedModels()
	if len(models) == 0 {
		t.Error("expected non-empty supported models list")
	}

	// Should not include wildcard
	for _, model := range models {
		if model == "*" {
			t.Error("supported models should not include wildcard")
		}
	}

	// Should be a sorted list of canonical names of configured models
	expectedModels := []string{
		"deepseek-ai/DeepSeek-R1-0528",
		"meta-llama/Llama-3.3-70B",
		"zai-org/GLM-4.6",
		"dphn/Dolphin-Mistral-24B-Venice-Edition",
		"openai/gpt-4.1",
		"openai/gpt-5",
		"openai/gpt-5-pro",
		"openai/gpt-4",
		"openai/gpt-4-turbo",
		"openai/gpt-3.5-turbo",
		"openai/o1-preview",
		"openai/o1-mini",
		"openai/o3-mini",
	}

	sort.Strings(expectedModels)

	if len(expectedModels) != len(models) {
		t.Errorf("expected %d models, got %d", len(expectedModels), len(models))
	}

	for i, expected := range expectedModels {
		if models[i] != expected {
			t.Errorf("expected model %s, got %s", expected, models[i])
		}
	}
}

func TestGetProviders(t *testing.T) {
	providers := router.GetProviders()
	if len(providers) == 0 {
		t.Error("expected non-empty providers list")
	}

	// Should be a sorted list of names of configured providers that have any models routed
	// to them
	expectedProviders := []string{
		"Eternis",
		"Tinfoil",
		"OpenAI",
		"OpenRouter",
	}

	sort.Strings(expectedProviders)

	if len(expectedProviders) != len(providers) {
		t.Errorf("expected %d providers, got %d", len(expectedProviders), len(providers))
	}

	for i, expected := range expectedProviders {
		if providers[i] != expected {
			t.Errorf("expected provider %s, got %s", expected, providers[i])
		}
	}
}

func TestRouteModelWithWhitespace(t *testing.T) {
	provider, err := router.RouteModel("  gpt-4  ", "mobile")
	if err != nil {
		t.Fatalf("RouteModel failed for model with whitespace: %v", err)
	}
	if provider.Name != "OpenAI" {
		t.Errorf("expected OpenAI after trimming whitespace, got %s", provider.Name)
	}
}

func TestGetOpenRouterAPIKeyFallback(t *testing.T) {
	// Save the pre-initialized global configuration as the sub-tests will modify it
	// NOTE: in the current implementation, this test cannot be run in parallel to other
	// tests that use config.AppConfig to initialize new ModelRouters or use t.Setenv(),
	// as well as its sub-tests cannot be run in parallel.
	appConfig := config.AppConfig

	tests := []struct {
		name        string
		mobileKey   string
		desktopKey  string
		platform    string
		expectedKey string
	}{
		{
			name:        "mobile platform with both keys",
			mobileKey:   "mobile-key",
			desktopKey:  "desktop-key",
			platform:    "mobile",
			expectedKey: "mobile-key",
		},
		{
			name:        "desktop platform with both keys",
			mobileKey:   "mobile-key",
			desktopKey:  "desktop-key",
			platform:    "desktop",
			expectedKey: "desktop-key",
		},
		{
			name:        "only mobile key available",
			mobileKey:   "mobile-key",
			desktopKey:  "",
			platform:    "desktop",
			expectedKey: "mobile-key", // Falls back to mobile
		},
		{
			name:        "only desktop key available",
			mobileKey:   "",
			desktopKey:  "desktop-key",
			platform:    "mobile",
			expectedKey: "desktop-key", // Falls back to desktop
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Set OpenRouter keys to test-specific values
			for name, value := range Env {
				t.Setenv(name, value)
			}

			t.Setenv("OPENROUTER_DESKTOP_API_KEY", tt.desktopKey)
			t.Setenv("OPENROUTER_MOBILE_API_KEY", tt.mobileKey)

			// Regenerate the configuration for the test
			config.LoadConfig()
			router := NewModelRouter(config.AppConfig, log)

			// Test the resulting keys in a route
			provider, err := router.RouteModel("unknown-model", tt.platform)
			if err != nil {
				t.Fatalf("RouteModel failed: %v", err)
			}
			if provider.APIKey != tt.expectedKey {
				t.Errorf("expected key %s, got %s", tt.expectedKey, provider.APIKey)
			}
		})
	}

	// Restore the pre-initialized global configuration
	config.AppConfig = appConfig
}
