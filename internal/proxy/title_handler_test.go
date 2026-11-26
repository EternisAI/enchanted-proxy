package proxy

import (
	"errors"
	"log/slog"
	"testing"

	"github.com/eternisai/enchanted-proxy/internal/logger"
	"github.com/eternisai/enchanted-proxy/internal/routing"
)

type fakeTitleModels struct {
	configured bool
	endpoint   *routing.ProviderConfig
	err        error
}

func (f fakeTitleModels) HasTitleModel() bool { return f.configured }

func (f fakeTitleModels) GetTitleGenerationConfig() (*routing.ProviderConfig, error) {
	return f.endpoint, f.err
}

// A chat's title uses the configured title model; without one, or when it is
// unavailable, the chat's own model.
func TestChatTitleEndpoint(t *testing.T) {
	log := logger.New(logger.Config{Level: slog.LevelError})
	title := &routing.ProviderConfig{Name: "Tinfoil", Model: "gemma4-31b", BaseURL: "https://inference.tinfoil.sh/v1", APIKey: "title-key"}
	chat := []string{"gpt-5.5", "https://api.openai.com/v1", "chat-key"}

	cases := []struct {
		name   string
		titles fakeTitleModels
		want   []string
	}{
		{"no title model", fakeTitleModels{}, chat},
		{"title model", fakeTitleModels{configured: true, endpoint: title}, []string{"gemma4-31b", "https://inference.tinfoil.sh/v1", "title-key"}},
		{"title model unavailable", fakeTitleModels{configured: true, err: errors.New("no API key")}, chat},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			model, baseURL, apiKey := chatTitleEndpoint(log, tc.titles, chat[0], chat[1], chat[2])
			if got := []string{model, baseURL, apiKey}; got[0] != tc.want[0] || got[1] != tc.want[1] || got[2] != tc.want[2] {
				t.Fatalf("title endpoint %v, want %v", got, tc.want)
			}
		})
	}
}
