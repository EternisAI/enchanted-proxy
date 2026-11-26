package routing

import (
	"testing"

	"github.com/eternisai/enchanted-proxy/internal/config"
)

func withTitleModel(provider, model string) func(*config.Config) {
	return func(cfg *config.Config) {
		cfg.TitleGeneration = &config.TitleGenerationConfig{Provider: provider, Model: model}
	}
}

// The configured title model is used as given, on its provider's base URL and
// key, although it is not one of the models clients can request.
func TestConfiguredTitleModel(t *testing.T) {
	router := newModelRouter(t, newEnv(nil), withTitleModel("Tinfoil", "gemma4-31b"))

	if !router.HasTitleModel() {
		t.Fatal("HasTitleModel() = false with a title model configured")
	}
	got, err := router.GetTitleGenerationConfig()
	if err != nil {
		t.Fatalf("GetTitleGenerationConfig: %v", err)
	}
	want := ProviderConfig{
		Name:    "Tinfoil",
		BaseURL: TinfoilBaseURL,
		APIKey:  TinfoilAPIKey,
		Model:   "gemma4-31b",
		APIType: config.APITypeChatCompletions,
	}
	if *got != want {
		t.Fatalf("title endpoint %+v, want %+v", *got, want)
	}

	// Callers get a copy: changing it leaves the router's title model alone.
	got.Model = "changed"
	if again, _ := router.GetTitleGenerationConfig(); again.Model != "gemma4-31b" {
		t.Fatalf("title model became %q after a caller changed its copy", again.Model)
	}

	// Configuring it as the title model does not make it a chat model.
	if chat, err := router.RouteModel("gemma4-31b", "mobile"); err == nil && chat.Name == "Tinfoil" {
		t.Fatal("the title model became routable on Tinfoil for chats")
	}
}

// Without its provider's API key the title model is unavailable, and Kimi K2
// is not used in its place.
func TestTitleModelWithoutItsKey(t *testing.T) {
	router := newModelRouter(t, newEnv(map[string]string{TinfoilAPIKeyEnvVar: ""}), withTitleModel("Tinfoil", "gemma4-31b"))

	if !router.HasTitleModel() {
		t.Fatal("HasTitleModel() = false with a title model configured")
	}
	if got, err := router.GetTitleGenerationConfig(); err == nil {
		t.Fatalf("got title endpoint %+v without the provider's key, want an error", *got)
	}
}

// Without a title model, Responses API and deep research titles use Kimi K2
// (and chat titles keep each chat's model).
func TestDefaultTitleModel(t *testing.T) {
	router := newModelRouter(t, newEnv(nil))

	if router.HasTitleModel() {
		t.Fatal("HasTitleModel() = true with no title model configured")
	}
	got, err := router.GetTitleGenerationConfig()
	if err != nil {
		t.Fatalf("GetTitleGenerationConfig: %v", err)
	}
	if got.Name != "Tinfoil" || got.Model != "kimi-k2-6" {
		t.Fatalf("default title endpoint is %s on %s, want kimi-k2-6 on Tinfoil (the test config's Kimi K2)", got.Model, got.Name)
	}
}
