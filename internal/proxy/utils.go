package proxy

import (
	"encoding/json"
	"strings"

	"github.com/eternisai/enchanted-proxy/internal/config"
	"github.com/eternisai/enchanted-proxy/internal/tools"
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

// GetAPIKey returns the appropriate API key for a base URL and platform
func GetAPIKey(baseURL string, platform string, config *config.Config) string {
	switch baseURL {
	case "https://openrouter.ai/api/v1":
		return getOpenRouterAPIKey(platform, config)
	case "https://api.openai.com/v1":
		return config.OpenAIAPIKey
	case "https://inference.tinfoil.sh/v1":
		return config.TinfoilAPIKey
	case "https://cloud-api.near.ai/v1":
		return config.NearAPIKey
	case "http://127.0.0.1:20001/v1":
		return config.EternisInferenceAPIKey
	default:
		return ""
	}
}

// Usage represents token usage from OpenAI/OpenRouter APIs.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// CompletionResponse represents a non-streamed completion response.
type CompletionResponse struct {
	Usage *Usage `json:"usage"`
}

// extractTokenUsage extracts token usage from non-streamed OpenAI/OpenRouter response.
func extractTokenUsage(responseBody []byte) *Usage {
	if len(responseBody) == 0 {
		return nil
	}

	var parsed CompletionResponse
	if err := json.Unmarshal(responseBody, &parsed); err != nil {
		return nil
	}

	return parsed.Usage
}

// StreamChunk represents a single chunk in a streamed response.
type StreamChunk struct {
	Choices []interface{} `json:"choices"`
	Usage   *Usage        `json:"usage"`
}

// extractTokenUsageFromSSELine safely extracts token usage from a single SSE data line.
// Returns nil if no usage data is found or if parsing fails.
func extractTokenUsageFromSSELine(line string) *Usage {
	if !strings.HasPrefix(line, "data: ") {
		return nil
	}

	data := strings.TrimPrefix(line, "data: ")
	if data == "[DONE]" {
		return nil
	}

	var chunk map[string]interface{}
	if err := json.Unmarshal([]byte(data), &chunk); err != nil {
		return nil
	}

	usage, exists := chunk["usage"]
	if !exists || usage == nil {
		return nil
	}

	usageMap, ok := usage.(map[string]interface{})
	if !ok {
		return nil
	}

	promptTokens, ok1 := usageMap["prompt_tokens"].(float64)
	completionTokens, ok2 := usageMap["completion_tokens"].(float64)
	totalTokens, ok3 := usageMap["total_tokens"].(float64)

	if !ok1 || !ok2 || !ok3 {
		return nil
	}

	return &Usage{
		PromptTokens:     int(promptTokens),
		CompletionTokens: int(completionTokens),
		TotalTokens:      int(totalTokens),
	}
}

func getOpenRouterAPIKey(platform string, config *config.Config) string {
	switch platform {
	case "mobile":
		return config.OpenRouterMobileAPIKey
	case "desktop":
		return config.OpenRouterDesktopAPIKey
	default:
		return ""
	}
}

// injectToolsIntoRequest injects tool definitions into a chat completions request if not already present.
// Returns the modified request body or the original body if injection is not needed.
func injectToolsIntoRequest(requestBody []byte, toolRegistry *tools.Registry) ([]byte, error) {
	if toolRegistry == nil || len(requestBody) == 0 {
		return requestBody, nil
	}

	// Parse request body
	var requestData map[string]interface{}
	if err := json.Unmarshal(requestBody, &requestData); err != nil {
		return requestBody, err
	}

	// Check if tools are already present
	if _, exists := requestData["tools"]; exists {
		// Tools already defined by client, don't override
		return requestBody, nil
	}

	// Inject tool definitions
	toolDefs := toolRegistry.GetDefinitions()
	if len(toolDefs) > 0 {
		requestData["tools"] = toolDefs
	}

	// Re-marshal request
	modifiedBody, err := json.Marshal(requestData)
	if err != nil {
		return requestBody, err
	}

	return modifiedBody, nil
}
