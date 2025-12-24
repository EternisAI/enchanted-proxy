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

// isRetryableError checks if an error is transient and worth retrying
func isRetryableError(err error) bool {
	if err == nil {
		return false
	}
	errStr := err.Error()
	// Retry on network errors, timeouts, 5xx errors, rate limits
	return strings.Contains(errStr, "timeout") ||
		strings.Contains(errStr, "timed out") ||
		strings.Contains(errStr, "connection refused") ||
		strings.Contains(errStr, "connection reset") ||
		strings.Contains(errStr, "no such host") ||
		strings.Contains(errStr, "EOF") ||
		strings.Contains(errStr, "503") ||
		strings.Contains(errStr, "502") ||
		strings.Contains(errStr, "504") ||
		strings.Contains(errStr, "429") || // Rate limit
		strings.Contains(errStr, "500")
}

// GenerateTitle calls AI to generate a title from the first message with retry logic
func GenerateTitle(ctx context.Context, req TitleGenerationRequest, apiKey string) (string, error) {
	maxRetries := 3
	var lastErr error

	for attempt := 1; attempt <= maxRetries; attempt++ {
		title, err := generateTitleAttempt(ctx, req, apiKey)
		if err == nil {
			return title, nil
		}

		lastErr = err

		// Only retry on transient errors
		if isRetryableError(err) && attempt < maxRetries {
			// Exponential backoff: 1s, 2s
			backoffDuration := time.Duration(attempt) * time.Second
			select {
			case <-time.After(backoffDuration):
				continue
			case <-ctx.Done():
				return "", fmt.Errorf("context cancelled during retry: %w", ctx.Err())
			}
		}

		// Non-retryable error or final attempt
		break
	}

	return "", lastErr
}

// generateTitleAttempt is a single attempt to generate a title
func generateTitleAttempt(ctx context.Context, req TitleGenerationRequest, apiKey string) (string, error) {
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
	url := req.BaseURL + "/chat/completions"
	httpReq, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("failed to create request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+apiKey)

	// Execute with timeout
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("failed to call AI at %s: %w", url, err)
	}
	defer resp.Body.Close()

	// Read response body for debugging
	bodyBytes, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		return "", fmt.Errorf("failed to read response body: %w", readErr)
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("AI returned status %d: %s (url: %s, model: %s)", resp.StatusCode, string(bodyBytes), url, req.Model)
	}

	// Parse response
	var result struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}

	if err := json.Unmarshal(bodyBytes, &result); err != nil {
		return "", fmt.Errorf("failed to decode response: %w (response: %s)", err, string(bodyBytes))
	}

	if len(result.Choices) == 0 {
		return "", fmt.Errorf("no choices in response (response: %s)", string(bodyBytes))
	}

	title := strings.TrimSpace(result.Choices[0].Message.Content)

	// Clean up title (remove quotes if present)
	title = strings.Trim(title, `"'`)

	return title, nil
}
