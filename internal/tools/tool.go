package tools

import (
	"context"
	"encoding/json"
)

// Tool defines the interface for executable tools that AI models can call.
type Tool interface {
	// Name returns the unique identifier for this tool
	Name() string

	// Definition returns the OpenAI-compatible function definition
	Definition() ToolDefinition

	// Execute runs the tool with the given arguments
	// Returns formatted result string for AI consumption
	Execute(ctx context.Context, args string) (string, error)
}

// ToolDefinition represents an OpenAI-compatible function definition for tools.
type ToolDefinition struct {
	Type     string           `json:"type"` // Always "function"
	Function FunctionDef      `json:"function"`
}

// FunctionDef defines the function schema.
type FunctionDef struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description"`
	Parameters  map[string]interface{} `json:"parameters"`
}

// ToolCall represents a function call request from the AI.
type ToolCall struct {
	ID       string          `json:"id"`        // Unique call ID (e.g., "call_abc123")
	Type     string          `json:"type"`      // Always "function"
	Function ToolCallFunction `json:"function"`
}

// ToolCallFunction contains the function name and arguments.
type ToolCallFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"` // JSON string
}

// ToolResult represents the result of a tool execution.
type ToolResult struct {
	ToolCallID string `json:"tool_call_id"`
	Role       string `json:"role"` // Always "tool"
	Name       string `json:"name"` // Tool name
	Content    string `json:"content"`
}

// ParseArguments is a helper to parse JSON arguments into a struct.
func ParseArguments(args string, target interface{}) error {
	return json.Unmarshal([]byte(args), target)
}
