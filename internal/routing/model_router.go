package routing

import (
	"fmt"
	"log/slog"
	"strings"

	"github.com/eternisai/enchanted-proxy/internal/config"
	"github.com/eternisai/enchanted-proxy/internal/logger"
)

// ModelRouter handles automatic routing of model IDs to AI providers.
//
// Benefits over X-BASE-URL header:
//   - Clients don't need to maintain model-to-provider mapping
//   - Prevents misrouting (e.g., sending Claude model to OpenAI)
//   - Centralized configuration (update routing without client changes)
//   - Better error messages when model is unsupported
//
// Backward Compatibility:
//   - X-BASE-URL still supported during migration
//   - If X-BASE-URL provided: use it (legacy behavior)
//   - If X-BASE-URL missing: auto-route based on model (new behavior)
//
// Routing Strategy:
//  1. Exact match: "gpt-4" → OpenAI
//  2. Prefix match: "gpt-4-0125-preview" → OpenAI (prefix "gpt-4")
//  3. Fallback: Unknown models → OpenRouter
//
// Example Usage:
//
//	router := NewModelRouter(config, logger)
//	provider, err := router.RouteModel("gpt-4", "mobile")
//	// provider.BaseURL = "https://api.openai.com/v1"
//	// provider.APIKey = config.OpenAIAPIKey
type ModelRouter struct {
	routes map[string]ProviderConfig
	config *config.Config // Store config for platform-specific keys
	logger *logger.Logger
}

// APIType identifies which API format to use for a provider.
type APIType string

const (
	// APITypeChatCompletions uses the standard /chat/completions endpoint (OpenAI, Anthropic via OpenRouter, etc.)
	APITypeChatCompletions APIType = "chat_completions"

	// APITypeResponses uses OpenAI's stateful /responses endpoint (GPT-5 Pro, GPT-4.5+)
	APITypeResponses APIType = "responses"
)

// ProviderConfig contains routing information for an AI provider.
type ProviderConfig struct {
	// BaseURL is the base URL for the provider's API (e.g., "https://api.openai.com/v1")
	BaseURL string

	// APIKey is the API key for authentication
	APIKey string

	// Name is a human-readable provider name (e.g., "OpenAI", "Anthropic")
	Name string

	// APIType determines which API format to use (chat_completions or responses)
	APIType APIType
}

// NewModelRouter creates a new model router from configuration.
//
// Parameters:
//   - cfg: Application configuration (contains API keys)
//   - logger: Logger for routing decisions
//
// Returns:
//   - *ModelRouter: The new router with default routing table
//
// The router is initialized with default routes for common models.
// Platform-specific keys (OpenRouter) are resolved at route time.
func NewModelRouter(cfg *config.Config, logger *logger.Logger) *ModelRouter {
	routes := make(map[string]ProviderConfig)

	// OpenAI models - Chat Completions API
	if cfg.OpenAIAPIKey != "" {
		routes["gpt-4"] = ProviderConfig{
			BaseURL: "https://api.openai.com/v1",
			APIKey:  cfg.OpenAIAPIKey,
			Name:    "OpenAI",
			APIType: APITypeChatCompletions,
		}
		routes["gpt-4-turbo"] = ProviderConfig{
			BaseURL: "https://api.openai.com/v1",
			APIKey:  cfg.OpenAIAPIKey,
			Name:    "OpenAI",
			APIType: APITypeChatCompletions,
		}
		routes["gpt-3.5-turbo"] = ProviderConfig{
			BaseURL: "https://api.openai.com/v1",
			APIKey:  cfg.OpenAIAPIKey,
			Name:    "OpenAI",
			APIType: APITypeChatCompletions,
		}
		routes["o1-preview"] = ProviderConfig{
			BaseURL: "https://api.openai.com/v1",
			APIKey:  cfg.OpenAIAPIKey,
			Name:    "OpenAI",
			APIType: APITypeChatCompletions,
		}
		routes["o1-mini"] = ProviderConfig{
			BaseURL: "https://api.openai.com/v1",
			APIKey:  cfg.OpenAIAPIKey,
			Name:    "OpenAI",
			APIType: APITypeChatCompletions,
		}
		routes["o3-mini"] = ProviderConfig{
			BaseURL: "https://api.openai.com/v1",
			APIKey:  cfg.OpenAIAPIKey,
			Name:    "OpenAI",
			APIType: APITypeChatCompletions,
		}

		// OpenAI models - Responses API (stateful)
		routes["gpt-5-pro"] = ProviderConfig{
			BaseURL: "https://api.openai.com/v1",
			APIKey:  cfg.OpenAIAPIKey,
			Name:    "OpenAI",
			APIType: APITypeResponses,
		}
	}

	// Fallback: OpenRouter handles unknown models (including Claude via OpenRouter)
	// API key is resolved at route time based on platform (mobile/desktop)
	if cfg.OpenRouterMobileAPIKey != "" || cfg.OpenRouterDesktopAPIKey != "" {
		routes["*"] = ProviderConfig{
			BaseURL: "https://openrouter.ai/api/v1",
			APIKey:  "", // Resolved at route time based on platform
			Name:    "OpenRouter",
			APIType: APITypeChatCompletions, // OpenRouter uses Chat Completions format
		}
	}

	router := &ModelRouter{
		routes: routes,
		config: cfg,
		logger: logger,
	}

	logger.Info("model router initialized",
		slog.Int("route_count", len(routes)))

	return router
}

// RouteModel determines the provider for a given model ID.
//
// Parameters:
//   - modelID: The model identifier (e.g., "gpt-4", "claude-3-sonnet")
//   - platform: Client platform ("mobile", "desktop") - used for OpenRouter key selection
//
// Returns:
//   - *ProviderConfig: Provider configuration (baseURL, API key)
//   - error: If no provider configured for this model
//
// Routing algorithm:
//  1. Try exact match: routes["gpt-4"]
//  2. Try prefix match: "gpt-4-0125-preview" matches prefix "gpt-4"
//  3. Fall back to wildcard: routes["*"] (typically OpenRouter)
//  4. Error if no match found
//
// For OpenRouter fallback, the API key is selected based on platform:
//   - "mobile" → OpenRouterMobileAPIKey
//   - "desktop" → OpenRouterDesktopAPIKey
//   - default → OpenRouterMobileAPIKey
//
// Example:
//
//	provider, err := router.RouteModel("gpt-4-0125-preview", "mobile")
//	// Returns OpenAI provider (prefix match on "gpt-4")
func (mr *ModelRouter) RouteModel(modelID string, platform string) (*ProviderConfig, error) {
	if modelID == "" {
		return nil, fmt.Errorf("model ID is required")
	}

	// Normalize model ID (lowercase for comparison)
	normalizedModel := strings.ToLower(strings.TrimSpace(modelID))

	// Try exact match
	if config, exists := mr.routes[normalizedModel]; exists {
		// Make a copy to avoid modifying the original
		result := config
		// Resolve platform-specific API key if needed (OpenRouter)
		if result.Name == "OpenRouter" && result.APIKey == "" {
			result.APIKey = mr.getOpenRouterAPIKey(platform)
		}
		mr.logger.Debug("model routed (exact match)",
			slog.String("model", modelID),
			slog.String("provider", config.Name))
		return &result, nil
	}

	// Try prefix match
	// e.g., "gpt-4-0125-preview" should match "gpt-4"
	for prefix, config := range mr.routes {
		if prefix == "*" {
			continue // Skip wildcard for now
		}
		if strings.HasPrefix(normalizedModel, prefix) {
			// Make a copy to avoid modifying the original
			result := config
			// Resolve platform-specific API key if needed (OpenRouter)
			if result.Name == "OpenRouter" && result.APIKey == "" {
				result.APIKey = mr.getOpenRouterAPIKey(platform)
			}
			mr.logger.Debug("model routed (prefix match)",
				slog.String("model", modelID),
				slog.String("prefix", prefix),
				slog.String("provider", config.Name))
			return &result, nil
		}
	}

	// Fall back to wildcard (OpenRouter)
	if fallback, exists := mr.routes["*"]; exists {
		// Make a copy to avoid modifying the original
		result := fallback
		// Resolve platform-specific API key
		result.APIKey = mr.getOpenRouterAPIKey(platform)
		mr.logger.Info("model routed to fallback provider",
			slog.String("model", modelID),
			slog.String("provider", fallback.Name),
			slog.String("platform", platform))
		return &result, nil
	}

	// No provider configured
	return nil, fmt.Errorf("no provider configured for model: %s", modelID)
}

// getOpenRouterAPIKey returns the appropriate OpenRouter API key for the platform.
// Falls back to the other platform's key if the requested platform key is not configured.
func (mr *ModelRouter) getOpenRouterAPIKey(platform string) string {
	switch platform {
	case "mobile":
		if mr.config.OpenRouterMobileAPIKey != "" {
			return mr.config.OpenRouterMobileAPIKey
		}
		// Fall back to desktop key
		return mr.config.OpenRouterDesktopAPIKey
	case "desktop":
		if mr.config.OpenRouterDesktopAPIKey != "" {
			return mr.config.OpenRouterDesktopAPIKey
		}
		// Fall back to mobile key
		return mr.config.OpenRouterMobileAPIKey
	default:
		// Default to mobile if platform not specified
		if mr.config.OpenRouterMobileAPIKey != "" {
			return mr.config.OpenRouterMobileAPIKey
		}
		return mr.config.OpenRouterDesktopAPIKey
	}
}

// GetSupportedModels returns a list of explicitly configured models.
// Does not include wildcard "*".
//
// Returns:
//   - []string: List of supported model IDs
//
// Used for:
//   - Client model selection UI
//   - API documentation
//   - Health checks
func (mr *ModelRouter) GetSupportedModels() []string {
	models := make([]string, 0, len(mr.routes))
	for model := range mr.routes {
		if model != "*" {
			models = append(models, model)
		}
	}
	return models
}

// GetProviders returns a list of all configured providers.
// Useful for observability and debugging.
//
// Returns:
//   - []string: List of provider names (e.g., ["OpenAI", "Anthropic", "OpenRouter"])
func (mr *ModelRouter) GetProviders() []string {
	providerMap := make(map[string]bool)
	for _, config := range mr.routes {
		providerMap[config.Name] = true
	}

	providers := make([]string, 0, len(providerMap))
	for provider := range providerMap {
		providers = append(providers, provider)
	}
	return providers
}
