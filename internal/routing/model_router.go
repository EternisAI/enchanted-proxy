package routing

import (
	"errors"
	"fmt"
	"log/slog"
	"sort"
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
//	// provider.APIKey = os.Getenv("OPENAI_API_KEY")
type ModelRouter struct {
	aliases map[string]string
	routes  map[string]ModelRoute
	config  *config.Config // Store config for platform-specific keys
	logger  *logger.Logger
}

// ModelRoute maintains actual lists of provider endpoints where the requests for this model
// can be routed.
type ModelRoute struct {
	// ActiveEndpoints is the list of ModelEndpoints that are currently active and can accept
	// requests for this model.
	ActiveEndpoints []ModelEndpoint

	// InactiveEndpoints is the list of ModelEndpoints that are currently inactive and
	// should not be used to route requests for this model.
	// Endpoints may be deactivated by fallback policy or other similar settings.
	// TODO: Will be used by the fallback policy.
	InactiveEndpoints []ModelEndpoint
}

// ModelEndpoint contains all information necessary to route requests for a specific model to
// a specific inference API provider, aggregated from the declarative routing configuration.
type ModelEndpoint struct {
	Provider ProviderConfig
	// TODO: Fallback policy configuration will be stored here
}

// ProviderConfig contains aggregated routing information for an AI provider.
type ProviderConfig struct {
	// BaseURL is the base URL for the provider's API (e.g., "https://api.openai.com/v1")
	BaseURL string

	// APIKey is the API key for authentication
	APIKey string

	// Name is a human-readable provider name (e.g., "OpenAI", "Anthropic")
	Name string

	// Model is the name of the model that the provider expects in the API requests
	Model string

	// APIType determines which API format to use (chat_completions or responses)
	APIType config.APIType

	// TokenMultiplier is the cost multiplier for this model (1× to 50×)
	TokenMultiplier float64
}

// NewModelRouter creates a new model router from configuration.
//
// Parameters:
//   - cfg: Application configuration (contains model router configuration and OpenRouter API keys)
//   - logger: Logger for routing decisions
//
// Returns:
//   - *ModelRouter: The new router with a populated routing table
//
// The router is initialized with a routing table populated from the model router configuration
// which is included in the application configuration.
// Platform-specific keys (OpenRouter) are resolved at route time.
func NewModelRouter(cfg *config.Config, logger *logger.Logger) *ModelRouter {
	router := &ModelRouter{
		config: cfg,
		logger: logger,
	}

	router.RebuildRoutes(cfg.ModelRouterConfig)

	if len(router.routes) == 0 {
		logger.Error("model router has no model routes")
		return nil
	}

	logger.Info("model router initialized",
		slog.Int("route_count", len(router.routes)))

	return router
}

// RebuildRoutes updates the routing table and alias mapping in place by building it from the
// provided declarative configuration.
//
// Parameters:
//   - cfg: Model Router configuration
func (mr *ModelRouter) RebuildRoutes(cfg *config.ModelRouterConfig) {
	if cfg == nil {
		return
	}

	// Normally each model has at least one alias, so pre-allocate twice the number of items
	aliases := make(map[string]string, len(cfg.Models)*2)
	routes := make(map[string]ModelRoute, len(cfg.Models)*2)

	// Build a map of model providers configs
	providers := make(map[string]config.ModelProviderConfig, len(cfg.Providers))
	for _, modelProvider := range cfg.Providers {
		if _, exists := providers[modelProvider.Name]; exists {
			mr.logger.Warn("skipping duplicate provider config entry",
				slog.String("provider", modelProvider.Name))
			continue
		}
		providers[modelProvider.Name] = modelProvider
	}

	// For every model, build the list of available endpoints, aggregating provider-level and
	// model-level routing configuration (like BaseURL and model name overrides).
	for _, model := range cfg.Models {
		if _, exists := routes[model.Name]; exists {
			mr.logger.Warn("skipping duplicate model config entry",
				slog.String("model", model.Name))
		}

		var activeEndpoints []ModelEndpoint

		for _, endpointProvider := range model.Providers {
			if modelProvider, exists := providers[endpointProvider.Name]; exists {
				// Skip providers that do not have an API key properly configured
				if modelProvider.APIKey == "" && modelProvider.Name != "OpenRouter" {
					continue
				}

				// Build an aggregated provider configuration for this endpoint
				provider := ProviderConfig{
					BaseURL:         modelProvider.BaseURL,
					APIKey:          modelProvider.APIKey,
					Name:            modelProvider.Name,
					Model:           model.Name,
					APIType:         endpointProvider.APIType,
					TokenMultiplier: model.TokenMultiplier,
				}

				// Override the model name with the one expected by this provider for this model
				if endpointProvider.Model != "" {
					provider.Model = endpointProvider.Model
				}

				// Override the base URL with the one used by this provider for this model
				if endpointProvider.BaseURL != "" {
					provider.BaseURL = endpointProvider.BaseURL
				}

				endpoint := ModelEndpoint{provider}
				activeEndpoints = append(activeEndpoints, endpoint)
			} else {
				mr.logger.Warn("skipping unknown model endpoint provider",
					slog.String("model", model.Name),
					slog.String("provider", endpointProvider.Name))
				continue
			}
		}

		// Populate routes and alias mapping for the model.
		// Alias mapping entries are normaized for reliable matching.
		if len(activeEndpoints) > 0 {
			routes[model.Name] = ModelRoute{
				ActiveEndpoints: activeEndpoints,
			}

			aliases[strings.ToLower(strings.TrimSpace(model.Name))] = model.Name

			for _, alias := range model.Aliases {
				aliases[strings.ToLower(strings.TrimSpace(alias))] = model.Name
			}
		} else {
			mr.logger.Warn("skipping model with no configured provider endpoints",
				slog.String("model", model.Name))
		}
	}

	// Update the routing table and alias mappings in place
	mr.aliases = aliases
	mr.routes = routes
}

// RouteModel determines the provider for a given model ID.
//
// Parameters:
//   - modelID: The model identifier (e.g., "gpt-4", "claude-3-sonnet")
//   - platform: Client platform ("mobile", "desktop") - used for OpenRouter key selection
//
// Returns:
//   - *ProviderConfig: Aggregated provider configuration suitable for routing (baseURL, API key)
//   - error: If no suitable provider found for this model
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
		return nil, errors.New("model ID is required")
	}

	// Normalize model ID (lowercase for comparison)
	normalizedModel := strings.ToLower(strings.TrimSpace(modelID))

	// Try exact match
	if canonicalModel, exists := mr.aliases[normalizedModel]; exists {
		if provider := mr.getModelEndpointProvider(canonicalModel, platform); provider != nil {
			mr.logger.Debug("model routed (exact match)",
				slog.String("model", modelID),
				slog.String("provider", provider.Name))
			return provider, nil
		}
	}

	// Try prefix match
	// e.g., "gpt-4-0125-preview" should match "gpt-4"
	for prefix, canonicalModel := range mr.aliases {
		if prefix == "*" {
			continue // Skip wildcard for now
		}

		if strings.HasPrefix(normalizedModel, prefix) {
			if provider := mr.getModelEndpointProvider(canonicalModel, platform); provider != nil {
				mr.logger.Debug("model routed (prefix match)",
					slog.String("model", modelID),
					slog.String("prefix", prefix),
					slog.String("provider", provider.Name))
				return provider, nil
			}
		}
	}

	// Fall back to wildcard (OpenRouter)
	if provider := mr.getModelEndpointProvider("*", platform); provider != nil {
		mr.logger.Info("model routed to fallback provider",
			slog.String("model", modelID),
			slog.String("provider", provider.Name),
			slog.String("platform", platform))
		return provider, nil
	}

	// No suitable endpoint provider found
	return nil, fmt.Errorf("no suitable endpoint provider found for model: %s", modelID)
}

// getModelEndpointProvider returns a final aggregated provider configuration that will be used
// to send requests to this model.
//
// Parameters:
//   - model: The "canonical" name of the model
//   - platform: Client platform ("mobile", "desktop") - used for OpenRouter key selection
func (mr *ModelRouter) getModelEndpointProvider(model string, platform string) *ProviderConfig {
	route, exists := mr.routes[model]
	if !exists {
		return nil
	}

	// TODO: implement fallback logic
	// For now, just select the first endpoint on the list every time.

	if len(route.ActiveEndpoints) == 0 {
		return nil
	}

	endpoint := route.ActiveEndpoints[0]
	provider := endpoint.Provider

	// For OpenRouter, determine the API key dynamically based on the platform and update in
	// the selected provider endpoint configuration.
	// This list of endpoints contains values and we are updating and returning a copy.
	if provider.Name == "OpenRouter" {
		apiKey := mr.getOpenRouterAPIKey(platform)

		if apiKey == "" {
			mr.logger.Warn("no API key configured for OpenRouter")
			return nil
		}

		provider.APIKey = apiKey
	}

	return &provider
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
//   - []string: List of supported model IDs, sorted for stability of the results
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

	sort.Strings(models)

	return models
}

// GetProviders returns a list of all configured providers, sorted for stability of the results.
// Useful for observability and debugging.
//
// Returns:
//   - []string: List of provider names (e.g., ["Anthropic", "OpenAI", "OpenRouter"])
func (mr *ModelRouter) GetProviders() []string {
	providerMap := make(map[string]struct{})
	for _, route := range mr.routes {
		for _, endpoint := range route.ActiveEndpoints {
			providerMap[endpoint.Provider.Name] = struct{}{}
		}

		for _, endpoint := range route.InactiveEndpoints {
			providerMap[endpoint.Provider.Name] = struct{}{}
		}
	}

	providers := make([]string, 0, len(providerMap))
	for provider := range providerMap {
		providers = append(providers, provider)
	}

	sort.Strings(providers)

	return providers
}

// GetTitleGenerationConfig returns the provider configuration for title generation.
// Uses GLM 4.6 as the default model for cost-effective title generation.
//
// Returns:
//   - *ProviderConfig: GLM 4.6 provider config (model, baseURL, API key)
//   - error: If GLM 4.6 is not configured
//
// Used by:
//   - GPT-5 Pro responses (instead of expensive GPT-5 Pro for titles)
//   - Deep Research sessions (for initial chat title)
func (mr *ModelRouter) GetTitleGenerationConfig() (*ProviderConfig, error) {
	// Use GLM 4.6 for title generation (cost-effective, fast)
	// IMPORTANT: Use uppercase variant "zai-org/GLM-4.6" as that's the "canonical" name.
	if provider := mr.getModelEndpointProvider("zai-org/GLM-4.6", ""); provider == nil {
		return provider, nil
	} else {
		return nil, errors.New("could not find a suitable endpoint for GLM 4.6 for title generation")
	}
}
