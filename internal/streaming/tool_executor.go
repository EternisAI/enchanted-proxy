package streaming

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"

	"github.com/eternisai/enchanted-proxy/internal/logger"
	"github.com/eternisai/enchanted-proxy/internal/tools"
)

// ToolExecutor handles executing tool calls and creating continuation requests.
type ToolExecutor struct {
	registry       *tools.Registry
	logger         *logger.Logger
	httpClient     *http.Client
	upstreamURL    string
	upstreamAPIKey string
}

// ToolNotification represents a notification about tool execution.
type ToolNotification struct {
	Event      string `json:"event"`             // "started", "completed", "error"
	ToolName   string `json:"tool_name"`         // e.g., "exa_search"
	ToolCallID string `json:"tool_call_id"`      // e.g., "call_abc123"
	Summary    string `json:"summary,omitempty"` // Result summary (for completed)
	Error      string `json:"error,omitempty"`   // Error message (for error)
}

// NewToolExecutor creates a new tool executor.
func NewToolExecutor(
	registry *tools.Registry,
	logger *logger.Logger,
	upstreamURL string,
	upstreamAPIKey string,
) *ToolExecutor {
	return &ToolExecutor{
		registry:       registry,
		logger:         logger.WithComponent("tool-executor"),
		httpClient:     &http.Client{},
		upstreamURL:    upstreamURL,
		upstreamAPIKey: upstreamAPIKey,
	}
}

// ExecuteToolCalls executes multiple tool calls in parallel.
// Returns tool results and notifications for broadcasting.
func (te *ToolExecutor) ExecuteToolCalls(
	ctx context.Context,
	chatID, messageID string,
	toolCalls []tools.ToolCall,
) ([]tools.ToolResult, []ToolNotification, error) {
	if len(toolCalls) == 0 {
		return nil, nil, nil
	}

	te.logger.Info("executing tool calls",
		slog.String("chat_id", chatID),
		slog.String("message_id", messageID),
		slog.Int("count", len(toolCalls)))

	results := make([]tools.ToolResult, len(toolCalls))
	notifications := make([]ToolNotification, 0, len(toolCalls)*2) // *2 for started + (completed|error)
	var notifMu sync.Mutex
	var wg sync.WaitGroup
	var mu sync.Mutex
	errors := make([]error, 0)

	// Execute all tool calls in parallel
	for i, toolCall := range toolCalls {
		wg.Add(1)
		go func(idx int, tc tools.ToolCall) {
			defer wg.Done()

			// Notify started
			notifMu.Lock()
			notifications = append(notifications, ToolNotification{
				Event:      "started",
				ToolName:   tc.Function.Name,
				ToolCallID: tc.ID,
			})
			notifMu.Unlock()

			// Execute tool
			result, err := te.executeSingleTool(ctx, tc)
			if err != nil {
				te.logger.Error("tool execution failed",
					slog.String("tool_name", tc.Function.Name),
					slog.String("tool_call_id", tc.ID),
					slog.String("error", err.Error()))

				// Notify error
				notifMu.Lock()
				notifications = append(notifications, ToolNotification{
					Event:      "error",
					ToolName:   tc.Function.Name,
					ToolCallID: tc.ID,
					Error:      err.Error(),
				})
				notifMu.Unlock()

				mu.Lock()
				errors = append(errors, fmt.Errorf("tool %s: %w", tc.Function.Name, err))
				mu.Unlock()

				// Return error message as tool result
				result = tools.ToolResult{
					ToolCallID: tc.ID,
					Role:       "tool",
					Name:       tc.Function.Name,
					Content:    fmt.Sprintf("Error executing tool: %s", err.Error()),
				}
			} else {
				// Notify completed
				notifMu.Lock()
				notifications = append(notifications, ToolNotification{
					Event:      "completed",
					ToolName:   tc.Function.Name,
					ToolCallID: tc.ID,
					Summary:    te.getSummary(result.Content),
				})
				notifMu.Unlock()
			}

			results[idx] = result
		}(i, toolCall)
	}

	wg.Wait()

	if len(errors) > 0 {
		te.logger.Warn("some tool calls failed",
			slog.Int("failed_count", len(errors)),
			slog.Int("total_count", len(toolCalls)))
	}

	return results, notifications, nil
}

// executeSingleTool executes a single tool call.
func (te *ToolExecutor) executeSingleTool(ctx context.Context, toolCall tools.ToolCall) (tools.ToolResult, error) {
	// Get tool from registry
	tool, exists := te.registry.Get(toolCall.Function.Name)
	if !exists {
		return tools.ToolResult{}, fmt.Errorf("tool %s not found", toolCall.Function.Name)
	}

	// Execute tool
	content, err := tool.Execute(ctx, toolCall.Function.Arguments)
	if err != nil {
		return tools.ToolResult{}, err
	}

	return tools.ToolResult{
		ToolCallID: toolCall.ID,
		Role:       "tool",
		Name:       toolCall.Function.Name,
		Content:    content,
	}, nil
}

// getSummary creates a short summary of the tool result.
func (te *ToolExecutor) getSummary(content string) string {
	const maxLen = 100
	if len(content) <= maxLen {
		return content
	}
	return content[:maxLen] + "..."
}

// CreateContinuationRequest creates a new AI request with tool results.
// This sends the tool results back to the AI and gets a new streaming response.
func (te *ToolExecutor) CreateContinuationRequest(
	ctx context.Context,
	originalMessages []interface{},
	assistantMessage map[string]interface{},
	toolResults []tools.ToolResult,
	toolDefinitions []tools.ToolDefinition,
) (io.ReadCloser, error) {
	// Build new messages array: original messages + assistant message + tool results
	messages := make([]interface{}, 0, len(originalMessages)+1+len(toolResults))
	messages = append(messages, originalMessages...)
	messages = append(messages, assistantMessage)

	// Add tool results
	for _, result := range toolResults {
		messages = append(messages, map[string]interface{}{
			"role":         result.Role,
			"content":      result.Content,
			"tool_call_id": result.ToolCallID,
		})
	}

	// Create request payload
	payload := map[string]interface{}{
		"messages": messages,
		"stream":   true,
	}

	// Include tools if provided (so AI can call tools again if needed)
	if len(toolDefinitions) > 0 {
		payload["tools"] = toolDefinitions
	}

	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal payload: %w", err)
	}

	te.logger.Debug("creating continuation request",
		slog.Int("messages", len(messages)),
		slog.Int("tool_results", len(toolResults)))

	// Create HTTP request
	req, err := http.NewRequestWithContext(ctx, "POST", te.upstreamURL, bytes.NewBuffer(payloadBytes))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+te.upstreamAPIKey)

	// Execute request
	resp, err := te.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to execute request: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, fmt.Errorf("upstream returned status %d: %s", resp.StatusCode, string(body))
	}

	return resp.Body, nil
}
