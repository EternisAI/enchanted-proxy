package routing

import (
	"log/slog"
	"os"
	"sort"
	"sync/atomic"
	"testing"

	"github.com/eternisai/enchanted-proxy/internal/config"
	"github.com/eternisai/enchanted-proxy/internal/logger"
)

var (
	ConfigFileEnvVar              = "CONFIG_FILE"
	EternisAPIKeyEnvVar           = "ETERNIS_INFERENCE_API_KEY"
	NearAIAPIKeyEnvVar            = "NEAR_API_KEY"
	OpenAIAPIKeyEnvVar            = "OPENAI_API_KEY"
	OpenRouterMobileAPIKeyEnvVar  = "OPENROUTER_MOBILE_API_KEY"
	OpenRouterDesktopAPIKeyEnvVar = "OPENROUTER_DESKTOP_API_KEY"
	TinfoilAPIKeyEnvVar           = "TINFOIL_API_KEY"

	ConfigFile              = "testdata/config.yaml"
	EternisAPIKey           = "test-eternis-key"
	NearAIAPIKey            = "test-near-ai-key"
	OpenAIAPIKey            = "test-openai-key"
	OpenRouterDesktopAPIKey = "test-openrouter-desktop-key"
	OpenRouterMobileAPIKey  = "test-openrouter-mobile-key"
	TinfoilAPIKey           = "test-tinfoil-key"

	EternisGLM46BaseURL   = "http://127.0.0.1:20001/v1"
	EternisMistralBaseURL = "http://34.30.193.13:8000/v1"
	OpenAIBaseURL         = "https://api.openai.com/v1"
	OpenRouterBaseURL     = "https://openrouter.ai/api/v1"
	TinfoilBaseURL        = "https://inference.tinfoil.sh/v1"
)

func newEnv(overrides map[string]string) map[string]string {
	env := map[string]string{
		ConfigFileEnvVar:              ConfigFile,
		EternisAPIKeyEnvVar:           EternisAPIKey,
		NearAIAPIKeyEnvVar:            NearAIAPIKey,
		OpenAIAPIKeyEnvVar:            OpenAIAPIKey,
		OpenRouterMobileAPIKeyEnvVar:  OpenRouterMobileAPIKey,
		OpenRouterDesktopAPIKeyEnvVar: OpenRouterDesktopAPIKey,
		TinfoilAPIKeyEnvVar:           TinfoilAPIKey,
	}

	for key, value := range overrides {
		env[key] = value
	}

	return env
}

func newModelRouter(t *testing.T, env map[string]string) *ModelRouter {
	var log *logger.Logger
	if testing.Verbose() {
		log = logger.New(logger.Config{Level: slog.LevelDebug})
	} else {
		log = logger.New(logger.Config{Level: slog.LevelError})
	}

	for key, value := range env {
		t.Setenv(key, value)
	}

	appConfig := &config.Config{
		OpenRouterMobileAPIKey:  os.Getenv(OpenRouterMobileAPIKeyEnvVar),
		OpenRouterDesktopAPIKey: os.Getenv(OpenRouterDesktopAPIKeyEnvVar),
	}

	configFilePath := os.Getenv(ConfigFileEnvVar)
	configFile, err := os.Open(configFilePath)
	defer func() {
		if configFile != nil {
			configFile.Close()
		}
	}()

	if err != nil {
		t.Fatalf("Failed to open config file: %v", err)
	}

	if err := config.LoadConfigFile(configFile, appConfig); err != nil {
		t.Fatalf("Failed to load config file: %v", err)
	}

	return NewModelRouter(appConfig, log)
}

func TestNewModelRouter(t *testing.T) {
	router := newModelRouter(t, newEnv(nil))

	if router == nil {
		t.Fatal("NewModelRouter returned nil")
	}

	routes := router.GetRoutes()
	if len(routes) == 0 {
		t.Fatal("routes map is empty")
	}

	if router.getOpenRouterAPIKey("desktop") != OpenRouterDesktopAPIKey ||
		router.getOpenRouterAPIKey("mobile") != OpenRouterMobileAPIKey {
		t.Fatal("platform api keys for OpenRouter are not processed correctly")
	}
}

func TestRouteModelExactMatch(t *testing.T) {
	router := newModelRouter(t, newEnv(nil))

	tests := []struct {
		model            string
		expectedBaseURL  string
		expectedKey      string
		expectedAPIType  config.APIType
		expectedProvider string
	}{
		// To ensure the "exact" match, use the "canonical" model names and the models that
		// don't have model name overrides on the endpoint configuration level in the testdata.
		{"zai-org/GLM-4.6", EternisGLM46BaseURL, EternisAPIKey, config.APITypeChatCompletions, "Eternis"},
		{"dphn/Dolphin-Mistral-24B-Venice-Edition", EternisMistralBaseURL, EternisAPIKey, config.APITypeChatCompletions, "Eternis"},
		{"openai/gpt-4.1", OpenRouterBaseURL, OpenRouterMobileAPIKey, config.APITypeChatCompletions, "OpenRouter"},
		{"openai/gpt-5", OpenRouterBaseURL, OpenRouterMobileAPIKey, config.APITypeChatCompletions, "OpenRouter"},
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

func TestRouteModelTokenMultiplier(t *testing.T) {
	router := newModelRouter(t, newEnv(nil))

	tests := map[string]float64{
		"gpt-4": 1.0,
		"gpt-5": 6.0,
	}

	for model, expectedTokenMultiplier := range tests {
		t.Run(model, func(t *testing.T) {
			provider, err := router.RouteModel(model, "")
			if err != nil {
				t.Fatalf("RouteModel failed: %v", err)
			}

			if provider.TokenMultiplier != expectedTokenMultiplier {
				t.Errorf(
					"expected TokenMultiplier %v, got %v",
					expectedTokenMultiplier,
					provider.TokenMultiplier,
				)
			}
		})
	}
}

func TestRouteModelBaseURLOverride(t *testing.T) {
	router := newModelRouter(t, newEnv(nil))

	provider, err := router.RouteModel("zai-org/GLM-4.6", "")
	if err != nil {
		t.Fatalf("RouteModel failed: %v", err)
	}

	if provider.BaseURL != EternisGLM46BaseURL {
		t.Errorf("expected BaseURL %s, got %s", EternisGLM46BaseURL, provider.BaseURL)
	}
}

func TestRouteModelNameOverride(t *testing.T) {
	router := newModelRouter(t, newEnv(nil))

	provider, err := router.RouteModel("deepseek-ai/DeepSeek-R1-0528", "")
	if err != nil {
		t.Fatalf("RouteModel failed: %v", err)
	}

	expectedModel := "deepseek-r1-0528"
	if provider.Model != expectedModel {
		t.Errorf("expected model name %s, got %s", expectedModel, provider.BaseURL)
	}
}

func TestRouteModelAPITypeOverride(t *testing.T) {
	router := newModelRouter(t, newEnv(nil))

	tests := map[string]config.APIType{
		"gpt-4":     config.APITypeChatCompletions,
		"gpt-5-pro": config.APITypeResponses,
	}

	for model, expectedAPIType := range tests {
		t.Run(model, func(t *testing.T) {
			provider, err := router.RouteModel(model, "")
			if err != nil {
				t.Fatalf("RouteModel failed: %v", err)
			}

			if provider.APIType != expectedAPIType {
				t.Errorf("expected API type %s, got %s", expectedAPIType, provider.APIType)
			}
		})
	}
}

func TestRouteModelAliasMatch(t *testing.T) {
	router := newModelRouter(t, newEnv(nil))

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
	router := newModelRouter(t, newEnv(nil))

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
	router := newModelRouter(t, newEnv(nil))

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
			if provider.BaseURL != OpenRouterBaseURL {
				t.Errorf("expected OpenRouter baseURL, got %s", provider.BaseURL)
			}
			if provider.APIKey != OpenRouterMobileAPIKey {
				t.Errorf("expected mobile key, got %s", provider.APIKey)
			}
		})
	}
}

func TestRouteModelPlatformSpecificKeys(t *testing.T) {
	router := newModelRouter(t, newEnv(nil))

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
	router := newModelRouter(t, newEnv(nil))

	_, err := router.RouteModel("", "mobile")
	if err == nil {
		t.Error("expected error for empty model ID")
	}
}

func TestRouteModelNoProviderConfigured(t *testing.T) {
	router := newModelRouter(t, map[string]string{
		ConfigFileEnvVar: ConfigFile,
	})

	provider, err := router.RouteModel("gpt-4", "mobile")
	if err == nil {
		t.Errorf("expected error when no provider keys are configured, got %v", provider.Name)
	}
}

func TestRouteModelCaseInsensitive(t *testing.T) {
	router := newModelRouter(t, newEnv(nil))

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
	router := newModelRouter(t, newEnv(nil))

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
	router := newModelRouter(t, newEnv(nil))

	providers := router.GetProviders()
	if len(providers) == 0 {
		t.Error("expected non-empty providers list")
	}

	// Should be a sorted list of names of configured providers that have any models routed
	// to them
	expectedProviders := []string{
		"Eternis",
		"NEAR AI",
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
	router := newModelRouter(t, newEnv(nil))

	provider, err := router.RouteModel("  gpt-4  ", "mobile")
	if err != nil {
		t.Fatalf("RouteModel failed for model with whitespace: %v", err)
	}
	if provider.Name != "OpenAI" {
		t.Errorf("expected OpenAI after trimming whitespace, got %s", provider.Name)
	}
}

func TestGetOpenRouterAPIKeyFallback(t *testing.T) {
	tests := []struct {
		name        string
		mobileKey   string
		desktopKey  string
		platform    string
		expectedKey string
	}{
		{
			name:        "mobile platform with both keys",
			mobileKey:   OpenRouterMobileAPIKey,
			desktopKey:  OpenRouterDesktopAPIKey,
			platform:    "mobile",
			expectedKey: OpenRouterMobileAPIKey,
		},
		{
			name:        "desktop platform with both keys",
			mobileKey:   OpenRouterMobileAPIKey,
			desktopKey:  OpenRouterDesktopAPIKey,
			platform:    "desktop",
			expectedKey: OpenRouterDesktopAPIKey,
		},
		{
			name:        "only mobile key available",
			mobileKey:   OpenRouterMobileAPIKey,
			desktopKey:  "",
			platform:    "desktop",
			expectedKey: OpenRouterMobileAPIKey, // Falls back to mobile
		},
		{
			name:        "only desktop key available",
			mobileKey:   "",
			desktopKey:  OpenRouterDesktopAPIKey,
			platform:    "mobile",
			expectedKey: OpenRouterDesktopAPIKey, // Falls back to desktop
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			router := newModelRouter(t, newEnv(map[string]string{
				OpenRouterMobileAPIKeyEnvVar:  tt.mobileKey,
				OpenRouterDesktopAPIKeyEnvVar: tt.desktopKey,
			}))
			provider, err := router.RouteModel("unknown-model", tt.platform)
			if err != nil {
				t.Fatalf("RouteModel failed: %v", err)
			}
			if provider.APIKey != tt.expectedKey {
				t.Errorf("expected key %s, got %s", tt.expectedKey, provider.APIKey)
			}
		})
	}
}

func TestFallbackEndpoints(t *testing.T) {
	router := newModelRouter(t, newEnv(nil))
	routes := router.GetRoutes()

	model := "zai-org/GLM-4.6"
	route, ok := routes[model]
	if !ok {
		t.Fatalf("No route for model %s", model)
	}

	if len(route.ActiveEndpoints) != 1 {
		t.Errorf("Expected 1 active endpoint, got %d", len(route.ActiveEndpoints))
	}

	if len(route.InactiveEndpoints) != 1 {
		t.Errorf("Expected 1 inactive endpoint, got %d", len(route.InactiveEndpoints))
	}
}

func TestRoundRobinRouting(t *testing.T) {
	router := newModelRouter(t, newEnv(nil))
	routes := router.GetRoutes()

	model := "zai-org/GLM-4.6"
	route, ok := routes[model]
	if !ok {
		t.Fatalf("No route for model %s", model)
	}

	activeEndpoints := append(route.ActiveEndpoints, route.InactiveEndpoints...)
	if len(activeEndpoints) != 2 {
		t.Errorf("Expected 2 endpoints in total, got %d", len(route.ActiveEndpoints))
	}

	newRoutes := make(map[string]ModelRoute, len(routes))
	for key, value := range routes {
		newRoutes[key] = value
	}

	newRoutes[model] = ModelRoute{
		ActiveEndpoints:   activeEndpoints,
		RoundRobinCounter: &atomic.Uint64{},
	}

	router.SetRoutes(newRoutes)

	tests := []string{"Eternis", "NEAR AI", "Eternis", "NEAR AI", "Eternis"}
	for n, expectedProvider := range tests {
		provider, err := router.RouteModel(model, "")
		if err != nil {
			t.Fatalf("RouteModel failed: %v", err)
		}
		if provider.Name != expectedProvider {
			t.Errorf("Expected provider %s on attempt #%d, got %s", expectedProvider, n+1, provider.Name)
		}
	}
}

func TestPanicModeRouting(t *testing.T) {
	router := newModelRouter(t, newEnv(nil))
	routes := router.GetRoutes()

	model := "zai-org/GLM-4.6"
	route, ok := routes[model]
	if !ok {
		t.Fatalf("No route for model %s", model)
	}

	inactiveEndpoints := append(route.ActiveEndpoints, route.InactiveEndpoints...)
	if len(inactiveEndpoints) != 2 {
		t.Errorf("Expected 2 endpoints in total, got %d", len(route.ActiveEndpoints))
	}

	newRoutes := make(map[string]ModelRoute, len(routes))
	for key, value := range routes {
		newRoutes[key] = value
	}

	newRoutes[model] = ModelRoute{
		InactiveEndpoints: inactiveEndpoints,
		RoundRobinCounter: &atomic.Uint64{},
	}

	router.SetRoutes(newRoutes)

	tests := []string{"Eternis", "NEAR AI", "Eternis", "NEAR AI", "Eternis"}
	for n, expectedProvider := range tests {
		provider, err := router.RouteModel(model, "")
		if err != nil {
			t.Fatalf("RouteModel failed: %v", err)
		}
		if provider.Name != expectedProvider {
			t.Errorf("Expected provider %s on attempt #%d, got %s", expectedProvider, n+1, provider.Name)
		}
	}
}
