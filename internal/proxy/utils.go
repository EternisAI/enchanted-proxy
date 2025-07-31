package proxy

import (
	"encoding/json"

	"github.com/eternisai/enchanted-proxy/internal/config"
)

// extractModelFromRequestBody extracts the model field from request body bytes.
func extractModelFromRequestBody(path string, body []byte) string {
	if path != "/chat/completions" {
		return ""
	}

	if len(body) == 0 {
		return ""
	}

	var requestData struct {
		Model string `json:"model"`
	}

	if err := json.Unmarshal(body, &requestData); err != nil {
		return ""
	}

	return requestData.Model
}

// logRequestBody safely logs relevant parts of the request body for debugging.
func logRequestBody(body []byte, maxSize int) string {
	if len(body) == 0 {
		return ""
	}

	bodyStr := string(body)
	if len(bodyStr) <= maxSize {
		return bodyStr
	}

	return bodyStr[:maxSize] + "..."
}

func getAPIKey(baseURL string, config *config.Config) string {
	switch baseURL {
	case "https://openrouter.ai/api/v1":
		return config.OpenRouterAPIKey
	case "https://api.openai.com/v1":
		return config.OpenAIAPIKey
	case "https://inference.tinfoil.sh/v1":
		return config.TinfoilAPIKey
	default:
		return ""
	}
}
