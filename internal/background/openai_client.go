package background

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/eternisai/enchanted-proxy/internal/logger"
	"log/slog"
)

// OpenAIClient handles polling OpenAI's Responses API for background responses.
type OpenAIClient struct {
	apiKey     string
	baseURL    string
	httpClient *http.Client
	logger     *logger.Logger
}

// NewOpenAIClient creates a new OpenAI polling client.
func NewOpenAIClient(apiKey, baseURL string, logger *logger.Logger) *OpenAIClient {
	return &OpenAIClient{
		apiKey:  apiKey,
		baseURL: baseURL,
		httpClient: &http.Client{
			Timeout: 5 * time.Minute, // Longer timeout for fetching large response content
			// Context timeout (30 min) will override if request takes longer
		},
		logger: logger,
	}
}

// GetResponseStatus polls OpenAI for the status of a background response.
//
// Parameters:
//   - ctx: Context for the request
//   - responseID: The response ID returned from the initial background request
//
// Returns:
//   - *ResponseStatus: The current status of the response
//   - error: If polling failed
//
// Example response from OpenAI:
//
//	{"id": "resp_abc123", "status": "in_progress", "created_at": 1234567890}
func (c *OpenAIClient) GetResponseStatus(ctx context.Context, responseID string) (*ResponseStatus, error) {
	// Note: baseURL already includes /v1, so we just append /responses/{id}
	url := fmt.Sprintf("%s/responses/%s", c.baseURL, responseID)

	c.logger.Debug("polling OpenAI response status",
		slog.String("response_id", responseID),
		slog.String("url", url))

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to poll OpenAI: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		c.logger.Error("OpenAI polling request failed",
			slog.Int("status_code", resp.StatusCode),
			slog.String("response_id", responseID),
			slog.String("url", url))
		return nil, fmt.Errorf("OpenAI returned status %d", resp.StatusCode)
	}

	var status ResponseStatus
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	c.logger.Debug("polled OpenAI response status",
		slog.String("response_id", responseID),
		slog.String("status", status.Status))

	return &status, nil
}

// GetResponseContent fetches the full content of a completed background response.
//
// This should only be called when status = "completed".
//
// Parameters:
//   - ctx: Context for the request
//   - responseID: The response ID
//
// Returns:
//   - *ResponseContent: The full response content with choices
//   - error: If fetching failed
func (c *OpenAIClient) GetResponseContent(ctx context.Context, responseID string) (*ResponseContent, error) {
	// Note: baseURL already includes /v1, so we just append /responses/{id}
	url := fmt.Sprintf("%s/responses/%s", c.baseURL, responseID)

	c.logger.Info("fetching completed response from OpenAI API",
		slog.String("response_id", responseID),
		slog.String("url", url))

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch response: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		c.logger.Error("OpenAI content fetch returned non-200 status",
			slog.Int("status_code", resp.StatusCode),
			slog.String("response_id", responseID),
			slog.String("url", url))
		return nil, fmt.Errorf("OpenAI returned status %d", resp.StatusCode)
	}

	var content ResponseContent
	if err := json.NewDecoder(resp.Body).Decode(&content); err != nil {
		return nil, fmt.Errorf("failed to decode response content: %w", err)
	}

	// Log response structure for debugging
	c.logger.Info("fetched completed response content",
		slog.String("response_id", responseID),
		slog.String("status", content.Status),
		slog.Int("output_items", len(content.Output)),
		slog.Int("choices", len(content.Choices)))

	return &content, nil
}

// ExtractContent extracts the text content from a ResponseContent.
//
// Parameters:
//   - content: The response content from OpenAI Responses API
//
// Returns:
//   - string: The extracted message content
//
// Responses API format (primary):
//
//	{"output": [
//	  {"type": "reasoning", "id": "rs_xxx..."},
//	  {"type": "message", "status": "completed", "content": [{"type": "output_text", "text": "Hello"}]}
//	]}
//
// Legacy Chat Completions format (fallback):
//
//	{"choices": [{"message": {"content": "Hello world"}}]}
func ExtractContent(content *ResponseContent) string {
	// Try Responses API format first (output array)
	if len(content.Output) > 0 {
		// Iterate through output items to find message with completed status
		for _, item := range content.Output {
			itemType, _ := item["type"].(string)
			if itemType == "message" {
				// Found a message item - extract content array
				if contentArray, ok := item["content"].([]interface{}); ok {
					for _, contentItem := range contentArray {
						if contentMap, ok := contentItem.(map[string]interface{}); ok {
							contentType, _ := contentMap["type"].(string)
							if contentType == "output_text" {
								if text, ok := contentMap["text"].(string); ok {
									return text
								}
							}
						}
					}
				}

				// Fallback: try direct content field
				if contentStr, ok := item["content"].(string); ok {
					return contentStr
				}
			}
		}
	}

	// Fallback: Try legacy Chat Completions format (choices array)
	if len(content.Choices) > 0 {
		choice := content.Choices[0]

		// Try to extract from message.content
		if message, ok := choice["message"].(map[string]interface{}); ok {
			if contentStr, ok := message["content"].(string); ok {
				return contentStr
			}
		}

		// Try delta.content (streaming format)
		if delta, ok := choice["delta"].(map[string]interface{}); ok {
			if contentStr, ok := delta["content"].(string); ok {
				return contentStr
			}
		}
	}

	return ""
}
