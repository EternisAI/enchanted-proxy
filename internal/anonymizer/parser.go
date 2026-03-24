package anonymizer

import (
	"encoding/json"
	"fmt"
	"strings"
)

// toolCallResponse is the JSON structure inside <tool_call> tags.
type toolCallResponse struct {
	Name      string `json:"name"`
	Arguments struct {
		Replacements []Replacement `json:"replacements"`
	} `json:"arguments"`
}

// ParseResponse extracts PII replacements from the anonymizer model's response content.
// The model returns content like:
//
//	<think>\n\n</think>\n\n<tool_call>\n{"name": "replace_entities", "arguments": {"replacements": [...]}}\n</tool_call>
func ParseResponse(content string) ([]Replacement, error) {
	// Extract content between <tool_call> and </tool_call>
	start := strings.Index(content, "<tool_call>")
	end := strings.Index(content, "</tool_call>")

	if start == -1 || end == -1 || end <= start {
		// No tool call found — model didn't detect any PII.
		// This happens when the model answers directly instead of calling the tool.
		return nil, nil
	}

	toolCallJSON := strings.TrimSpace(content[start+len("<tool_call>") : end])

	var tc toolCallResponse
	if err := json.Unmarshal([]byte(toolCallJSON), &tc); err != nil {
		return nil, fmt.Errorf("failed to parse tool_call JSON: %w", err)
	}

	return tc.Arguments.Replacements, nil
}
