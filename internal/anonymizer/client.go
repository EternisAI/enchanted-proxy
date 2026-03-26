package anonymizer

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

const (
	defaultModel      = "eternisai/Anonymizer-4B"
	defaultMaxTokens  = 250
	defaultTemp       = 0.3
	defaultTopP       = 0.9
	defaultTimeout    = 10 * time.Second
	completionsPath   = "/v1/chat/completions"
)

// ClientConfig holds configuration for the anonymizer HTTP client.
type ClientConfig struct {
	BaseURL string // e.g. "http://127.0.0.1:20120" or staging test URL
	APIKey  string
	Timeout time.Duration
}

// Client calls the Anonymizer CVM's chat completions endpoint.
type Client struct {
	baseURL    string
	apiKey     string
	httpClient *http.Client
}

// NewClient creates a new anonymizer client.
func NewClient(cfg ClientConfig) *Client {
	timeout := cfg.Timeout
	if timeout == 0 {
		timeout = defaultTimeout
	}

	return &Client{
		baseURL: cfg.BaseURL,
		apiKey:  cfg.APIKey,
		httpClient: &http.Client{
			Timeout: timeout,
		},
	}
}

// chatRequest is the OpenAI-compatible request body.
type chatRequest struct {
	Model       string        `json:"model"`
	MaxTokens   int           `json:"max_tokens"`
	Temperature float64       `json:"temperature"`
	TopP        float64       `json:"top_p"`
	Messages    []chatMessage `json:"messages"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// chatResponse is the relevant subset of the OpenAI-compatible response.
type chatResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
}

// Call sends a user message to the anonymizer and returns the raw response content.
func (c *Client) Call(ctx context.Context, userText string) (string, error) {
	body := chatRequest{
		Model:       defaultModel,
		MaxTokens:   defaultMaxTokens,
		Temperature: defaultTemp,
		TopP:        defaultTopP,
		Messages: []chatMessage{
			{Role: "user", Content: BuildPrompt(userText)},
		},
	}

	payload, err := json.Marshal(body)
	if err != nil {
		return "", fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+completionsPath, bytes.NewReader(payload))
	if err != nil {
		return "", fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.apiKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("anonymizer request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("anonymizer returned status %d: %s", resp.StatusCode, string(respBody))
	}

	var chatResp chatResponse
	if err := json.Unmarshal(respBody, &chatResp); err != nil {
		return "", fmt.Errorf("unmarshal response: %w", err)
	}

	if len(chatResp.Choices) == 0 {
		return "", fmt.Errorf("anonymizer returned no choices")
	}

	return chatResp.Choices[0].Message.Content, nil
}
