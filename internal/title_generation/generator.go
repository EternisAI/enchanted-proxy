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

	"github.com/eternisai/enchanted-proxy/internal/config"
)

const (
	maxRetries      = 3
	requestTimeout  = 30 * time.Second
	maxTokens       = 1000
	temperature     = 0.7
	contextTemplate = `First user message: %s

AI response: %s

Second user message: %s`
)

// Generator handles title generation via AI
type Generator struct {
	initialPrompt      string
	regenerationPrompt string
}

// NewGenerator creates a new title generator with prompts from config
func NewGenerator(cfg *config.TitleGenerationConfig) *Generator {
	return &Generator{
		initialPrompt:      strings.TrimSpace(cfg.InitialPrompt),
		regenerationPrompt: strings.TrimSpace(cfg.RegenerationPrompt),
	}
}

// GenerateInitial generates a title from the first user message
func (g *Generator) GenerateInitial(ctx context.Context, req GenerateRequest) (string, error) {
	return g.generate(ctx, g.initialPrompt, req.UserContent, req)
}

// GenerateFromContext generates a title using conversation context
func (g *Generator) GenerateFromContext(ctx context.Context, req GenerateRequest, regenCtx RegenerationContext) (string, error) {
	userContent := fmt.Sprintf(contextTemplate,
		regenCtx.FirstUserMessage,
		regenCtx.FirstAIResponse,
		regenCtx.SecondUserMessage,
	)
	return g.generate(ctx, g.regenerationPrompt, userContent, req)
}

// generate is the core generation function with retry logic
func (g *Generator) generate(ctx context.Context, systemPrompt, userContent string, req GenerateRequest) (string, error) {
	var lastErr error

	for attempt := 1; attempt <= maxRetries; attempt++ {
		title, err := g.callAI(ctx, systemPrompt, userContent, req)
		if err == nil {
			return title, nil
		}

		lastErr = err

		if isRetryableError(err) && attempt < maxRetries {
			backoff := time.Duration(attempt) * time.Second
			select {
			case <-time.After(backoff):
				continue
			case <-ctx.Done():
				return "", fmt.Errorf("context cancelled during retry: %w", ctx.Err())
			}
		}
		break
	}

	return "", lastErr
}

// callAI makes a single API call to generate a title
func (g *Generator) callAI(ctx context.Context, systemPrompt, userContent string, req GenerateRequest) (string, error) {
	payload := map[string]interface{}{
		"model": req.Model,
		"messages": []map[string]string{
			{"role": "system", "content": systemPrompt},
			{"role": "user", "content": userContent},
		},
		"max_tokens":  maxTokens,
		"temperature": temperature,
		"stream":      false,
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("marshal request: %w", err)
	}

	url := req.BaseURL + "/chat/completions"
	httpReq, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("create request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+req.APIKey)

	client := &http.Client{Timeout: requestTimeout}
	resp, err := client.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("call AI at %s: %w", url, err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("AI returned %d: %s (url: %s, model: %s)",
			resp.StatusCode, string(respBody), url, req.Model)
	}

	var result struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}

	if err := json.Unmarshal(respBody, &result); err != nil {
		return "", fmt.Errorf("decode response: %w (body: %s)", err, string(respBody))
	}

	if len(result.Choices) == 0 {
		return "", fmt.Errorf("no choices in response (body: %s)", string(respBody))
	}

	title := strings.TrimSpace(result.Choices[0].Message.Content)
	title = strings.Trim(title, `"'`)

	return title, nil
}

// isRetryableError checks if an error is transient and worth retrying
func isRetryableError(err error) bool {
	if err == nil {
		return false
	}
	errStr := err.Error()
	retryablePatterns := []string{
		"timeout", "timed out", "connection refused", "connection reset",
		"no such host", "EOF", "503", "502", "504", "429", "500",
	}
	for _, pattern := range retryablePatterns {
		if strings.Contains(errStr, pattern) {
			return true
		}
	}
	return false
}
