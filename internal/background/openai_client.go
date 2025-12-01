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
			Timeout: 30 * time.Second, // Short timeout for polling requests
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
//   {"id": "resp_abc123", "status": "in_progress", "created_at": 1234567890}
func (c *OpenAIClient) GetResponseStatus(ctx context.Context, responseID string) (*ResponseStatus, error) {
	url := fmt.Sprintf("%s/v1/responses/%s", c.baseURL, responseID)

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
		c.logger.Error("OpenAI polling returned error",
			slog.Int("status_code", resp.StatusCode),
			slog.String("response_id", responseID))
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
	url := fmt.Sprintf("%s/v1/responses/%s", c.baseURL, responseID)

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
		c.logger.Error("failed to fetch completed response",
			slog.Int("status_code", resp.StatusCode),
			slog.String("response_id", responseID))
		return nil, fmt.Errorf("OpenAI returned status %d", resp.StatusCode)
	}

	var content ResponseContent
	if err := json.NewDecoder(resp.Body).Decode(&content); err != nil {
		return nil, fmt.Errorf("failed to decode response content: %w", err)
	}

	c.logger.Info("fetched completed response content",
		slog.String("response_id", responseID),
		slog.String("status", content.Status),
		slog.Int("choices", len(content.Choices)))

	return &content, nil
}

// ExtractContent extracts the text content from a ResponseContent.
//
// Parameters:
//   - content: The response content from OpenAI
//
// Returns:
//   - string: The extracted message content
//
// Example OpenAI response structure:
//   {"choices": [{"message": {"content": "Hello world"}}]}
func ExtractContent(content *ResponseContent) string {
	if len(content.Choices) == 0 {
		return ""
	}

	choice := content.Choices[0]

	// Try to extract from message.content (non-streaming format)
	if message, ok := choice["message"].(map[string]interface{}); ok {
		if contentStr, ok := message["content"].(string); ok {
			return contentStr
		}
	}

	// Try to extract from other formats if needed
	// (OpenAI format may vary)

	return ""
}
