package proxy

import "github.com/eternisai/enchanted-proxy/internal/config"

func getAPIKey(baseURL string, config *config.Config) string {
	switch baseURL {
	case "https://openrouter.ai/api/v1":
		return config.OpenRouterAPIKey
	case "https://api.openai.com/v1":
		return config.OpenAIAPIKey
	case "https://inference.tinfoil.sh/v1":
		return config.TinfoilAPIKey
	}
	return ""
}
