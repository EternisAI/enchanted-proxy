package streaming

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/eternisai/enchanted-proxy/internal/logger"
	"github.com/eternisai/enchanted-proxy/internal/tools"
)

// ToolExecutor handles executing tool calls and creating continuation requests.
type ToolExecutor struct {
	registry   *tools.Registry
	logger     *logger.Logger
	httpClient *http.Client
}

// ToolNotification represents a notification about tool execution.
type ToolNotification struct {
	Event      string `json:"event"`             // "started", "completed", "error"
	ToolName   string `json:"tool_name"`         // e.g., "exa_search"
	ToolCallID string `json:"tool_call_id"`      // e.g., "call_abc123"
	Query      string `json:"query,omitempty"`   // Tool-specific query (e.g., search query)
	Summary    string `json:"summary,omitempty"` // Result summary (for completed)
	Error      string `json:"error,omitempty"`   // Error message (for error)
}

// NewToolExecutor creates a new tool executor.
func NewToolExecutor(
	registry *tools.Registry,
	logger *logger.Logger,
) *ToolExecutor {
	return &ToolExecutor{
		registry:   registry,
		logger:     logger.WithComponent("tool-executor"),
		httpClient: &http.Client{Timeout: 2 * time.Minute},
	}
}

// NotificationCallback is called when a tool execution event occurs.
// This allows real-time notification broadcasting instead of batching.
type NotificationCallback func(ToolNotification)

// ExecuteToolCalls executes multiple tool calls in parallel.
// The onNotification callback is called immediately when events occur (started, completed, error).
// Returns tool results only (notifications sent via callback).
func (te *ToolExecutor) ExecuteToolCalls(
	ctx context.Context,
	chatID, messageID string,
	toolCalls []tools.ToolCall,
	onNotification NotificationCallback,
) ([]tools.ToolResult, error) {
	if len(toolCalls) == 0 {
		return nil, nil
	}

	te.logger.Info("executing tool calls",
		slog.String("chat_id", chatID),
		slog.String("message_id", messageID),
		slog.Int("count", len(toolCalls)))

	results := make([]tools.ToolResult, len(toolCalls))
	var wg sync.WaitGroup
	var mu sync.Mutex
	errors := make([]error, 0)

	// Execute all tool calls in parallel
	for i, toolCall := range toolCalls {
		wg.Add(1)
		go func(idx int, tc tools.ToolCall) {
			defer wg.Done()

			// Notify started IMMEDIATELY via callback
			if onNotification != nil {
				onNotification(ToolNotification{
					Event:      "started",
					ToolName:   tc.Function.Name,
					ToolCallID: tc.ID,
				})
			}

			// Execute tool
			result, err := te.executeSingleTool(ctx, tc)
			if err != nil {
				te.logger.Error("tool execution failed",
					slog.String("tool_name", tc.Function.Name),
					slog.String("tool_call_id", tc.ID),
					slog.String("error", err.Error()))

				// Notify error IMMEDIATELY via callback
				if onNotification != nil {
					onNotification(ToolNotification{
						Event:      "error",
						ToolName:   tc.Function.Name,
						ToolCallID: tc.ID,
						Error:      err.Error(),
					})
				}

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
				// Notify completed IMMEDIATELY via callback
				if onNotification != nil {
					onNotification(ToolNotification{
						Event:      "completed",
						ToolName:   tc.Function.Name,
						ToolCallID: tc.ID,
						Query:      te.extractQuery(tc.Function.Name, tc.Function.Arguments),
						Summary:    te.getSummary(result.Content),
					})
				}
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

	return results, nil
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

// extractQuery extracts a human-readable query from tool arguments.
func (te *ToolExecutor) extractQuery(toolName, args string) string {
	switch toolName {
	case "web_search":
		var searchArgs struct {
			Queries []string `json:"queries"`
		}
		if err := json.Unmarshal([]byte(args), &searchArgs); err == nil && len(searchArgs.Queries) > 0 {
			return strings.Join(searchArgs.Queries, ", ")
		}
	case "search_memory":
		var memoryArgs struct {
			Query string `json:"query"`
		}
		if err := json.Unmarshal([]byte(args), &memoryArgs); err == nil && memoryArgs.Query != "" {
			return memoryArgs.Query
		}
	}
	return ""
}

// CreateContinuationRequest creates a new AI request with tool results.
// This sends the tool results back to the AI and gets a new streaming response.
func (te *ToolExecutor) CreateContinuationRequest(
	ctx context.Context,
	upstreamURL string,
	upstreamAPIKey string,
	originalReq map[string]interface{},
	originalMessages []interface{},
	assistantMessage map[string]interface{},
	toolResults []tools.ToolResult,
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

	// Create continuation payload by copying all original params
	payload := make(map[string]interface{})
	for k, v := range originalReq {
		// Skip messages (we rebuild it), stream (force true), and tools (we add them below)
		if k != "messages" && k != "stream" && k != "tools" {
			payload[k] = v
		}
	}

	// Set updated messages and ensure streaming
	payload["messages"] = messages
	payload["stream"] = true

	// Extract model from request for capability check
	modelID := ""
	if modelField, ok := payload["model"].(string); ok {
		modelID = modelField
	}

	// Include tool definitions in continuation requests if model supports them
	// This is necessary because the assistant message contains tool_calls,
	// and the AI provider needs the tool definitions to understand the context
	if tools.SupportsTools(modelID) {
		toolDefs := te.registry.GetDefinitions()
		if len(toolDefs) > 0 {
			payload["tools"] = toolDefs
			te.logger.Debug("included tool definitions in continuation",
				slog.Int("tool_count", len(toolDefs)),
				slog.String("model", modelID))
		}
	} else {
		te.logger.Debug("skipped tool definitions in continuation for model without support",
			slog.String("model", modelID))
	}

	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal payload: %w", err)
	}

	te.logger.Debug("creating continuation request",
		slog.String("upstream_url", upstreamURL),
		slog.Int("messages", len(messages)),
		slog.Int("tool_results", len(toolResults)))

	// Determine final URL: append /chat/completions if not already present
	finalURL := upstreamURL
	if !strings.HasSuffix(upstreamURL, "/chat/completions") {
		// Trim trailing slash and append endpoint
		finalURL = strings.TrimSuffix(upstreamURL, "/") + "/chat/completions"
	}

	te.logger.Debug("continuation request URL",
		slog.String("final_url", finalURL))

	// Create HTTP request
	req, err := http.NewRequestWithContext(ctx, "POST", finalURL, bytes.NewBuffer(payloadBytes))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+upstreamAPIKey)

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
