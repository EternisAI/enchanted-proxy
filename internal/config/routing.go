package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"time"

	"github.com/goccy/go-yaml"
)

// APIType identifies which API format to use for a provider.
type APIType string

const (
	// APITypeChatCompletions uses the standard /chat/completions endpoint (OpenAI, Anthropic via OpenRouter, etc.)
	APITypeChatCompletions APIType = "chat_completions"

	// APITypeResponses uses OpenAI's stateful /responses endpoint (GPT-5 Pro, GPT-4.5+)
	APITypeResponses APIType = "responses"
)

// Validate performs basic validation of an APIType value:
// - Checks whether the value is a known APIType
// - Replaces an empty value with the default one (APITypeChatCompletions)
func (t *APIType) Validate() error {
	switch *t {
	case "":
		*t = APITypeChatCompletions
		return nil
	case APITypeChatCompletions, APITypeResponses:
		return nil
	default:
		return fmt.Errorf(
			"bad APIType value: must be empty or one of %q, %q",
			string(APITypeChatCompletions),
			string(APITypeResponses),
		)
	}
}

// unmarshalAPITypeYAML implements a custom YAML unmarshaler for APIType.
// Validates the value after unmarshaling.
func unmarshalAPITypeYAML(value *APIType, data []byte) error {
	var apiType string

	if err := yaml.Unmarshal(data, &apiType); err != nil {
		return err
	}

	*value = APIType(apiType)

	if err := value.Validate(); err != nil {
		return err
	}

	return nil
}

// ModelRouterConfig contains configuration for routing.ModelRouter used to build an actual
// model routing table.
type ModelRouterConfig struct {
	// Providers contain configuration for inference API providers.
	Providers []ModelProviderConfig `yaml:"providers"`

	// Models contain routing configuration for models supported by our API.
	Models []ModelConfig `yaml:"models"`
}

// Validate performs validation of a ModelRouterConfig value:
// - Checks that provider and model lists are not empty
// - Checks that models reference known providers
// - Checks for duplicates in the lists of providers and models
func (cfg *ModelRouterConfig) Validate() error {
	if len(cfg.Providers) == 0 {
		return errors.New("no providers specified in model router configuration")
	}

	providers := make(map[string]struct{}, len(cfg.Providers))
	for _, provider := range cfg.Providers {
		if _, exists := providers[provider.Name]; exists {
			return fmt.Errorf("duplicate configuration entry for provider %v", provider.Name)
		}

		providers[provider.Name] = struct{}{}
	}

	if len(cfg.Models) == 0 {
		return errors.New("no models specified in model router configuration")
	}

	models := make(map[string]struct{}, len(cfg.Models))
	for _, model := range cfg.Models {
		for _, provider := range model.Providers {
			if _, providerExists := providers[provider.Name]; !providerExists {
				return fmt.Errorf("unknown provider %v specified for model %v", provider, model)
			}
		}

		if _, modelExists := models[model.Name]; modelExists {
			return fmt.Errorf("duplicate configuration entry for model %v", model.Name)
		}

		models[model.Name] = struct{}{}
	}

	return nil
}

// unmarshalModelRouterConfig implements a custom YAML unmarshaler for ModelRouterConfig.
// Validates the value after unmarshaling.
func unmarshalModelRouterConfig(value *ModelRouterConfig, data []byte) error {
	type Aux ModelRouterConfig
	var aux Aux

	if err := yaml.Unmarshal(data, &aux); err != nil {
		return err
	}

	*value = ModelRouterConfig(aux)

	if err := value.Validate(); err != nil {
		return err
	}

	return nil
}

// ModelProviderConfig contains basic configuration of an inference API provider.
type ModelProviderConfig struct {
	// Name is the human-readable name of the API provider.
	Name string `yaml:"name"`

	// BaseURL is the base URL for the provider's API (e.g., "https://api.openai.com/v1")
	// Can be empty if the provider uses different URLs for different models (like self-hosted),
	// otherwise must be a valid URL.
	BaseURL string `yaml:"base_url,omitempty"`

	// APIKeyEnvVar is the name of the environment variable that contains the API key.
	// Can be empty if the API key is resolved dynamically during routing (as for OpenRouter).
	APIKeyEnvVar string `yaml:"api_key_env_var,omitempty"`

	// APIKey is the actual API key used for authentication, extracted from the environment
	// using the APIKeyEnvVar value. Explicit config values are ignored.
	APIKey string `yaml:"-"`
}

// Validate performs validation of a ModelProviderConfig value:
// - Checks that the name is not empty
// - Verifies BaseURL is a valid URL
// - Fetches APIKey value from the environment using APIKeyEnvVar
func (cfg *ModelProviderConfig) Validate() error {
	if cfg.Name == "" {
		return errors.New("provider name must be specified in model provider configuration")
	}

	if err := validateURLString(cfg.BaseURL); err != nil {
		return err
	}

	if cfg.APIKeyEnvVar != "" {
		cfg.APIKey = os.Getenv(cfg.APIKeyEnvVar)
	}

	return nil
}

// unmarshalModelProviderConfig implements a custom YAML unmarshaler for ModelProviderConfig.
// Validates the value after unmarshaling.
func unmarshalModelProviderConfig(value *ModelProviderConfig, data []byte) error {
	type Aux ModelProviderConfig
	var aux Aux

	if err := yaml.Unmarshal(data, &aux); err != nil {
		return err
	}

	*value = ModelProviderConfig(aux)

	if err := value.Validate(); err != nil {
		return err
	}

	return nil
}

// ModelConfig contains routing configuration for a specific model supported by our API.
type ModelConfig struct {
	// Name is the full "canonical" name of the model.
	// HuggingFace repository paths or OpenRouter names are preferred as canonical model names.
	// This is the name of the model that will be set in the upstream request to the selected
	// API provider unless it's overridden by provider-specific configuration (see Providers).
	Name string `yaml:"name"`

	// Aliases is the list of alternative names accepted by our API.
	// They will be substituted with the actual model name (either the canonical one from Name
	// or the Model override in Providers).
	Aliases []string `yaml:"aliases,omitempty"`

	// TokenMultiplier is the token cost multiplier for this model (normally 0.5× to 50×).
	// Defaults to 1.0
	TokenMultiplier float64 `yaml:"token_multiplier,omitempty"`

	// Providers is the list of provider endpoint configurations that specify what providers
	// should be used to serve requests for this model and define necessary overrides.
	Providers []ModelEndpointProvider `yaml:"providers"`
}

// Validate performs validation of a ModelConfig value:
// - Checks that the name and the list of providers are not empty
// - Sets the default value of TokenMultiplier (1.0) if not specified
func (cfg *ModelConfig) Validate() error {
	if cfg.Name == "" {
		return errors.New("model name must be specified in model configuration")
	}

	if len(cfg.Providers) == 0 {
		return errors.New("no providers specified in model configuration")
	}

	if cfg.TokenMultiplier <= 0.0 {
		cfg.TokenMultiplier = 1.0
	}

	return nil
}

// unmarshalModelConfig implements a custom YAML unmarshaler for ModelConfig.
// Validates the value after unmarshaling.
func unmarshalModelConfig(value *ModelConfig, data []byte) error {
	type Aux ModelConfig
	var aux Aux

	if err := yaml.Unmarshal(data, &aux); err != nil {
		return err
	}

	*value = ModelConfig(aux)

	if err := value.Validate(); err != nil {
		return err
	}

	return nil
}

// ModelEndpointProvider contains settings of a specific model endpoint for a provider.
type ModelEndpointProvider struct {
	// Name is the name of the provider previously defined in ModelProviders.
	// Used to select the specific provider that will serve requests for this model.
	Name string `yaml:"name"`

	// Model is the name of the model that is expected by this provider.
	// Allows overriding the effective model name from the user input or the ModelConfig.
	Model string `yaml:"model,omitempty"`

	// BaseURL allows overriding the base URL specified in the ProviderConfig.
	// Should be a valid URL if present.
	BaseURL string `yaml:"base_url,omitempty"`

	// APIType determines which API format to use (chat_completions or responses).
	// Defaults to chat_completions.
	APIType APIType `yaml:"api_type,omitempty"`

	// FallbackConfig contains optional settings configuring traffic fallback behavior
	// for this provider endpoint if it becomes unhealthy or overloaded.
	Fallback *FallbackConfig `yaml:"fallback,omitempty"`
}

// Validate performs validation of a ModelEndpointProvider value:
// - Checks that the name is not empty
// - Verifies BaseURL is a valid URL
// - Sets the default value for APIType via validation
func (p *ModelEndpointProvider) Validate() error {
	if p.Name == "" {
		return errors.New("provider name must be specified in model endpoint configuration")
	}

	if err := validateURLString(p.BaseURL); err != nil {
		return err
	}

	if err := p.APIType.Validate(); err != nil {
		return err
	}

	if p.Fallback != nil {
		if err := p.Fallback.Validate(); err != nil {
			return err
		}
	}

	return nil
}

// unmarshalModelEndpointProvider implements a custom YAML unmarshaler for ModelEndpointProvider.
// Validates the value after unmarshaling.
func unmarshalModelEndpointProvider(value *ModelEndpointProvider, data []byte) error {
	type Aux ModelEndpointProvider
	var aux Aux

	if err := yaml.Unmarshal(data, &aux); err != nil {
		return err
	}

	*value = ModelEndpointProvider(aux)

	if err := value.Validate(); err != nil {
		return err
	}

	return nil
}

// Fallback contains fallback policy settings for a model endpoint
type FallbackConfig struct {
	// Trigger contains fallback policy settings for detecting an overload state that should
	// remove traffic from this endpoint.
	Trigger FallbackStateConfig `yaml:"trigger"`

	// Recover contains fallback policy setitings for detecting a recovery state that should
	// return traffic to this endpoint.
	Recover FallbackStateConfig `yaml:"recover"`
}

// Validate performs validation of a FallbackConfig value:
// - Checks that PromQL queries for trigger and recover events are specified
func (cfg *FallbackConfig) Validate() error {
	if cfg.Trigger.Query == "" {
		return errors.New("fallback trigger query must be specified")
	}

	if cfg.Recover.Query == "" {
		return errors.New("fallback recover query must be specified")
	}

	return nil
}

// unmarshalModelEndpointProvider implements a custom YAML unmarshaler for FallbackConfig.
// Validates the value after unmarshaling.
func unmarshalFallbackConfig(value *FallbackConfig, data []byte) error {
	type Aux FallbackConfig
	var aux Aux

	if err := yaml.Unmarshal(data, &aux); err != nil {
		return err
	}

	*value = FallbackConfig(aux)

	if err := value.Validate(); err != nil {
		return err
	}

	return nil
}

// FallbackStateConfig contains fallback policy configuration for a specific state of a model
// endpoint (overload/fallback or normal/recovery).
type FallbackStateConfig struct {
	// DwellTime is the duration of hysteresis period after entering the state which prevents
	// changing the state again for this duration.
	DwellTime time.Duration `yaml:"dwell_time"`

	// Query is a PromQL query that should return an empty vector or a vector of 0 while the
	// state is not entered and a vector of 1 after the state is entered
	Query string `yaml:"query"`
}

func init() {
	// Register unmarshalers of custom types with the YAML library
	yaml.RegisterCustomUnmarshaler[APIType](unmarshalAPITypeYAML)
	yaml.RegisterCustomUnmarshaler[ModelRouterConfig](unmarshalModelRouterConfig)
	yaml.RegisterCustomUnmarshaler[ModelProviderConfig](unmarshalModelProviderConfig)
	yaml.RegisterCustomUnmarshaler[ModelConfig](unmarshalModelConfig)
	yaml.RegisterCustomUnmarshaler[ModelEndpointProvider](unmarshalModelEndpointProvider)
	yaml.RegisterCustomUnmarshaler[FallbackConfig](unmarshalFallbackConfig)
}

// validateURLString performs basic sanity checks of a string that should contain a valid URL.
// Empty strings are ignored.
func validateURLString(str string) error {
	if str == "" {
		return nil
	}

	u, err := url.Parse(str)
	if err != nil {
		return fmt.Errorf("failed to parse URL: %w", err)
	}

	if u.Scheme != "https" && u.Scheme != "http" {
		return fmt.Errorf("unsupported URL scheme: %q", u.Scheme)
	}

	if u.Host == "" {
		return errors.New("URL does not contain a hostname")
	}

	return nil
}
