package streaming

import (
	"encoding/json"
	"strings"

	"github.com/eternisai/enchanted-proxy/internal/tools"
)

// ToolCallDetector detects and buffers tool calls from SSE stream chunks.
// It accumulates tool call data incrementally as chunks arrive.
type ToolCallDetector struct {
	toolCalls    map[int]*bufferedToolCall // Index -> tool call
	finishReason string
	hasToolCalls bool
}

// bufferedToolCall accumulates tool call data from chunks.
type bufferedToolCall struct {
	ID        string
	Type      string
	Name      string
	Arguments strings.Builder
}

// NewToolCallDetector creates a new tool call detector.
func NewToolCallDetector() *ToolCallDetector {
	return &ToolCallDetector{
		toolCalls: make(map[int]*bufferedToolCall),
	}
}

// ProcessChunk processes an SSE chunk and detects tool calls.
// Returns true if the chunk contains tool call data.
func (d *ToolCallDetector) ProcessChunk(line string) bool {
	// Parse SSE line
	if !strings.HasPrefix(line, "data: ") {
		return false
	}

	jsonData := strings.TrimPrefix(line, "data: ")
	if jsonData == "[DONE]" {
		return false
	}

	// Parse JSON
	var chunkData struct {
		Choices []struct {
			Delta struct {
				ToolCalls []struct {
					Index    int    `json:"index"`
					ID       string `json:"id"`
					Type     string `json:"type"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"delta"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}

	if err := json.Unmarshal([]byte(jsonData), &chunkData); err != nil {
		return false
	}

	if len(chunkData.Choices) == 0 {
		return false
	}

	choice := chunkData.Choices[0]

	// Check finish reason
	// Process tool calls BEFORE checking finish_reason.
	// Some providers (e.g., Kimi/Moonshot via Tinfoil) send tool_calls data
	// in the same chunk as finish_reason="tool_calls". We must accumulate
	// the arguments before potentially returning early.
	hasToolCallData := false
	if len(choice.Delta.ToolCalls) > 0 {
		d.hasToolCalls = true
		hasToolCallData = true

		for _, tc := range choice.Delta.ToolCalls {
			idx := tc.Index

			// Initialize tool call if new
			if _, exists := d.toolCalls[idx]; !exists {
				d.toolCalls[idx] = &bufferedToolCall{
					ID:   tc.ID,
					Type: tc.Type,
					Name: tc.Function.Name,
				}
			} else {
				// Update fields that may arrive in later chunks
				// Some providers (e.g., Kimi/Moonshot) may send ID or name in subsequent chunks
				if tc.ID != "" && d.toolCalls[idx].ID == "" {
					d.toolCalls[idx].ID = tc.ID
				}
				if tc.Function.Name != "" && d.toolCalls[idx].Name == "" {
					d.toolCalls[idx].Name = tc.Function.Name
				}
			}

			// Append arguments
			if tc.Function.Arguments != "" {
				d.toolCalls[idx].Arguments.WriteString(tc.Function.Arguments)
			}
		}
	}

	if choice.FinishReason != "" {
		d.finishReason = choice.FinishReason
		// If finish_reason is "tool_calls", this chunk should be suppressed
		if choice.FinishReason == "tool_calls" {
			return true
		}
	}

	return hasToolCallData
}

// IsComplete returns true if tool calls are complete (finish_reason = "tool_calls").
func (d *ToolCallDetector) IsComplete() bool {
	return d.finishReason == "tool_calls" && d.hasToolCalls
}

// GetToolCalls returns the detected tool calls.
func (d *ToolCallDetector) GetToolCalls() []tools.ToolCall {
	if !d.IsComplete() {
		return nil
	}

	result := make([]tools.ToolCall, 0, len(d.toolCalls))

	// Sort by index
	for i := 0; i < len(d.toolCalls); i++ {
		if tc, exists := d.toolCalls[i]; exists {
			result = append(result, tools.ToolCall{
				ID:   tc.ID,
				Type: tc.Type,
				Function: tools.ToolCallFunction{
					Name:      tc.Name,
					Arguments: tc.Arguments.String(),
				},
			})
		}
	}

	return result
}

// HasToolCalls returns true if any tool calls were detected.
func (d *ToolCallDetector) HasToolCalls() bool {
	return d.hasToolCalls
}
