package routing

import (
	"log/slog"
	"testing"

	"github.com/eternisai/enchanted-proxy/internal/config"
	"github.com/eternisai/enchanted-proxy/internal/logger"
)

func TestNewModelRouter(t *testing.T) {
	cfg := &config.Config{
		OpenAIAPIKey:            "test-openai-key",
		OpenRouterMobileAPIKey:  "test-openrouter-mobile-key",
		OpenRouterDesktopAPIKey: "test-openrouter-desktop-key",
	}
	log := logger.New(logger.Config{Level: slog.LevelError})

	router := NewModelRouter(cfg, log)
	if router == nil {
		t.Fatal("NewModelRouter returned nil")
	}
	if router.routes == nil {
		t.Fatal("routes map is nil")
	}
	if router.config != cfg {
		t.Error("config not stored correctly")
	}
}

func TestRouteModelExactMatch(t *testing.T) {
	cfg := &config.Config{
		OpenAIAPIKey:            "test-openai-key",
		OpenRouterMobileAPIKey:  "test-openrouter-key",
	}
	log := logger.New(logger.Config{Level: slog.LevelError})
	router := NewModelRouter(cfg, log)

	tests := []struct {
		model           string
		expectedBaseURL string
		expectedKey     string
		expectedAPIType APIType
	}{
		{"gpt-4", "https://api.openai.com/v1", "test-openai-key", APITypeChatCompletions},
		{"gpt-4-turbo", "https://api.openai.com/v1", "test-openai-key", APITypeChatCompletions},
		{"gpt-3.5-turbo", "https://api.openai.com/v1", "test-openai-key", APITypeChatCompletions},
		{"o1-preview", "https://api.openai.com/v1", "test-openai-key", APITypeChatCompletions},
		{"o1-mini", "https://api.openai.com/v1", "test-openai-key", APITypeChatCompletions},
		{"o3-mini", "https://api.openai.com/v1", "test-openai-key", APITypeChatCompletions},
		{"gpt-5-pro", "https://api.openai.com/v1", "test-openai-key", APITypeResponses},
	}

	for _, tt := range tests {
		t.Run(tt.model, func(t *testing.T) {
			provider, err := router.RouteModel(tt.model, "mobile")
			if err != nil {
				t.Fatalf("RouteModel failed: %v", err)
			}
			if provider.BaseURL != tt.expectedBaseURL {
				t.Errorf("expected baseURL %s, got %s", tt.expectedBaseURL, provider.BaseURL)
			}
			if provider.APIKey != tt.expectedKey {
				t.Errorf("expected API key %s, got %s", tt.expectedKey, provider.APIKey)
			}
			if provider.Name != "OpenAI" {
				t.Errorf("expected provider name 'OpenAI', got %s", provider.Name)
			}
			if provider.APIType != tt.expectedAPIType {
				t.Errorf("expected APIType %s, got %s", tt.expectedAPIType, provider.APIType)
			}
		})
	}
}

func TestRouteModelPrefixMatch(t *testing.T) {
	cfg := &config.Config{
		OpenAIAPIKey: "test-openai-key",
	}
	log := logger.New(logger.Config{Level: slog.LevelError})
	router := NewModelRouter(cfg, log)

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
	cfg := &config.Config{
		OpenAIAPIKey:           "test-openai-key",
		OpenRouterMobileAPIKey: "test-openrouter-mobile",
	}
	log := logger.New(logger.Config{Level: slog.LevelError})
	router := NewModelRouter(cfg, log)

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
			if provider.APIKey != "test-openrouter-mobile" {
				t.Errorf("expected mobile key, got %s", provider.APIKey)
			}
		})
	}
}

func TestRouteModelPlatformSpecificKeys(t *testing.T) {
	cfg := &config.Config{
		OpenRouterMobileAPIKey:  "mobile-key",
		OpenRouterDesktopAPIKey: "desktop-key",
	}
	log := logger.New(logger.Config{Level: slog.LevelError})
	router := NewModelRouter(cfg, log)

	tests := []struct {
		platform    string
		expectedKey string
	}{
		{"mobile", "mobile-key"},
		{"desktop", "desktop-key"},
		{"unknown", "mobile-key"}, // Falls back to mobile
		{"", "mobile-key"},         // Empty defaults to mobile
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
	cfg := &config.Config{
		OpenAIAPIKey: "test-key",
	}
	log := logger.New(logger.Config{Level: slog.LevelError})
	router := NewModelRouter(cfg, log)

	_, err := router.RouteModel("", "mobile")
	if err == nil {
		t.Error("expected error for empty model ID")
	}
}

func TestRouteModelNoProviderConfigured(t *testing.T) {
	cfg := &config.Config{
		// No API keys configured
	}
	log := logger.New(logger.Config{Level: slog.LevelError})
	router := NewModelRouter(cfg, log)

	_, err := router.RouteModel("gpt-4", "mobile")
	if err == nil {
		t.Error("expected error when no provider configured")
	}
}

func TestRouteModelCaseInsensitive(t *testing.T) {
	cfg := &config.Config{
		OpenAIAPIKey: "test-key",
	}
	log := logger.New(logger.Config{Level: slog.LevelError})
	router := NewModelRouter(cfg, log)

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
	cfg := &config.Config{
		OpenAIAPIKey:           "test-openai-key",
		OpenRouterMobileAPIKey: "test-openrouter-key",
	}
	log := logger.New(logger.Config{Level: slog.LevelError})
	router := NewModelRouter(cfg, log)

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

	// Should include known models
	expectedModels := []string{"gpt-4", "gpt-4-turbo", "gpt-3.5-turbo"}
	for _, expected := range expectedModels {
		found := false
		for _, model := range models {
			if model == expected {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected model %s not found in supported models", expected)
		}
	}
}

func TestGetProviders(t *testing.T) {
	cfg := &config.Config{
		OpenAIAPIKey:           "test-openai-key",
		OpenRouterMobileAPIKey: "test-openrouter-key",
	}
	log := logger.New(logger.Config{Level: slog.LevelError})
	router := NewModelRouter(cfg, log)

	providers := router.GetProviders()
	if len(providers) == 0 {
		t.Error("expected non-empty providers list")
	}

	// Should include OpenAI and OpenRouter
	hasOpenAI := false
	hasOpenRouter := false
	for _, provider := range providers {
		if provider == "OpenAI" {
			hasOpenAI = true
		}
		if provider == "OpenRouter" {
			hasOpenRouter = true
		}
	}

	if !hasOpenAI {
		t.Error("expected OpenAI in providers list")
	}
	if !hasOpenRouter {
		t.Error("expected OpenRouter in providers list")
	}
}

func TestRouteModelWithWhitespace(t *testing.T) {
	cfg := &config.Config{
		OpenAIAPIKey: "test-key",
	}
	log := logger.New(logger.Config{Level: slog.LevelError})
	router := NewModelRouter(cfg, log)

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
		name               string
		mobileKey          string
		desktopKey         string
		platform           string
		expectedKey        string
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
			cfg := &config.Config{
				OpenRouterMobileAPIKey:  tt.mobileKey,
				OpenRouterDesktopAPIKey: tt.desktopKey,
			}
			log := logger.New(logger.Config{Level: slog.LevelError})
			router := NewModelRouter(cfg, log)

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
