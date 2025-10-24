package title_generation

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const titleSystemPrompt = `You are a title generator. Generate a short, concise title for this conversation based on
the user's first message.

RULES:
- MAXIMUM 4 WORDS IN YOUR ANSWER
- TITLE MUST BE ON TOPIC
- USE PLAIN TEXT
- NO QUOTES
- NO MARKDOWN

NEVER BREAK RULES.

PRIORITIES:
1. RULES
2. USER'S REQUEST`

// GenerateTitle calls AI to generate a title from the first message
func GenerateTitle(ctx context.Context, req TitleGenerationRequest, apiKey string) (string, error) {
	// Build OpenAI-compatible request
	payload := map[string]interface{}{
		"model": req.Model,
		"messages": []map[string]string{
			{"role": "system", "content": titleSystemPrompt},
			{"role": "user", "content": req.FirstMessage},
		},
		"max_tokens":  1000,  // Title generation limit
		"temperature": 0.7,   // Some creativity
		"stream":      false, // Non-streaming
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("failed to marshal request: %w", err)
	}

	// Create HTTP request
	httpReq, err := http.NewRequestWithContext(ctx, "POST", req.BaseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("failed to create request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+apiKey)

	// Execute with timeout
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("failed to call AI: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("AI returned status %d: %s", resp.StatusCode, string(bodyBytes))
	}

	// Parse response
	var result struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("failed to decode response: %w", err)
	}

	if len(result.Choices) == 0 {
		return "", fmt.Errorf("no choices in response")
	}

	title := strings.TrimSpace(result.Choices[0].Message.Content)

	// Clean up title (remove quotes if present)
	title = strings.Trim(title, `"'`)

	return title, nil
}
