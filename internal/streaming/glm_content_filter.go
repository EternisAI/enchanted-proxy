package streaming

import (
	"encoding/json"
	"regexp"
	"strings"
)

// GLMContentFilter filters out <tool_call> XML tags from GLM model content streams.
//
// GLM 4.7 outputs tool calls as XML in the content field rather than structured JSON:
//
//	<tool_call>function_name
//	<arg_key>key1</arg_key>
//	<arg_value>value1</arg_value>
//	</tool_call>
//
// Known issues with GLM 4.7:
//   - Duplicated opening tags: <tool_call><tool_call><tool_call>Read
//   - Malformed/garbled tags mixed with content
//   - Intermittent missing <arg_key> sections
//
// This filter strips these XML tags from the content to prevent them from
// appearing in the UI while the proxy handles tool execution separately.
type GLMContentFilter struct {
	buffer           strings.Builder
	insideToolCall   bool
	partialTag       string
	toolCallsFound   []GLMToolCall
	toolCallComplete bool
}

// GLMToolCall represents a parsed GLM-style tool call
type GLMToolCall struct {
	Name      string
	Arguments map[string]string
}

// NewGLMContentFilter creates a new GLM content filter
func NewGLMContentFilter() *GLMContentFilter {
	return &GLMContentFilter{
		toolCallsFound: make([]GLMToolCall, 0),
	}
}

// Regex patterns for GLM tool call parsing
var (
	// Match opening tag (handles duplicates like <tool_call><tool_call><tool_call>)
	toolCallOpenPattern = regexp.MustCompile(`(<tool_call>)+`)

	// Match closing tag
	toolCallClosePattern = regexp.MustCompile(`</tool_call>`)

	// Match arg_key and arg_value pairs
	argPattern = regexp.MustCompile(`<arg_key>(.*?)</arg_key>\s*<arg_value>(.*?)</arg_value>`)

	// Match function name (first line after <tool_call>, before any <arg_key>)
	funcNamePattern = regexp.MustCompile(`^([^<\s]+)`)
)

// FilterContentChunk filters a content chunk, removing <tool_call> XML.
// Returns the filtered content (may be empty if entire chunk is tool call XML).
func (f *GLMContentFilter) FilterContentChunk(content string) string {
	// Combine with any partial tag from previous chunk
	fullContent := f.partialTag + content
	f.partialTag = ""

	var result strings.Builder
	pos := 0

	for pos < len(fullContent) {
		if f.insideToolCall {
			// Look for closing tag
			closeIdx := strings.Index(fullContent[pos:], "</tool_call>")
			if closeIdx != -1 {
				// Found closing tag - extract tool call content
				toolCallContent := fullContent[pos : pos+closeIdx]
				f.parseToolCallContent(toolCallContent)
				pos += closeIdx + len("</tool_call>")
				f.insideToolCall = false
				f.toolCallComplete = true
			} else {
				// No closing tag yet - check for partial
				if f.hasPartialClosingTag(fullContent[pos:]) {
					f.partialTag = fullContent[pos:]
				} else {
					// Buffer content inside tool call (will be parsed later)
					f.buffer.WriteString(fullContent[pos:])
				}
				break
			}
		} else {
			// Look for opening tag (handles duplicates)
			openMatch := toolCallOpenPattern.FindStringIndex(fullContent[pos:])
			if openMatch != nil {
				// Output content before the tool call
				result.WriteString(fullContent[pos : pos+openMatch[0]])
				pos += openMatch[1] // Skip past all duplicate <tool_call> tags
				f.insideToolCall = true
				f.buffer.Reset()
			} else {
				// Check for partial opening tag at end
				if f.hasPartialOpeningTag(fullContent[pos:]) {
					partialStart := f.findPartialTagStart(fullContent[pos:])
					result.WriteString(fullContent[pos : pos+partialStart])
					f.partialTag = fullContent[pos+partialStart:]
					break
				}
				// No tool call tags - output remaining content
				result.WriteString(fullContent[pos:])
				break
			}
		}
	}

	return result.String()
}

// parseToolCallContent extracts function name and arguments from tool call content
func (f *GLMContentFilter) parseToolCallContent(content string) {
	// Add any buffered content
	fullContent := f.buffer.String() + content
	f.buffer.Reset()

	// Clean up any duplicate <tool_call> prefixes in the function name
	fullContent = strings.ReplaceAll(fullContent, "<tool_call>", "")
	fullContent = strings.TrimSpace(fullContent)

	tc := GLMToolCall{
		Arguments: make(map[string]string),
	}

	// Extract function name (first non-whitespace before any tags)
	lines := strings.SplitN(fullContent, "\n", 2)
	if len(lines) > 0 {
		funcMatch := funcNamePattern.FindString(strings.TrimSpace(lines[0]))
		if funcMatch != "" {
			tc.Name = funcMatch
		}
	}

	// Extract arguments
	argMatches := argPattern.FindAllStringSubmatch(fullContent, -1)
	for _, match := range argMatches {
		if len(match) == 3 {
			tc.Arguments[match[1]] = match[2]
		}
	}

	if tc.Name != "" {
		f.toolCallsFound = append(f.toolCallsFound, tc)
	}
}

// hasPartialOpeningTag checks if content ends with a partial <tool_call> tag
func (f *GLMContentFilter) hasPartialOpeningTag(content string) bool {
	partials := []string{"<", "<t", "<to", "<too", "<tool", "<tool_", "<tool_c", "<tool_ca", "<tool_cal", "<tool_call"}
	for _, p := range partials {
		if strings.HasSuffix(content, p) {
			return true
		}
	}
	return false
}

// hasPartialClosingTag checks if content ends with a partial </tool_call> tag
func (f *GLMContentFilter) hasPartialClosingTag(content string) bool {
	partials := []string{"<", "</", "</t", "</to", "</too", "</tool", "</tool_", "</tool_c", "</tool_ca", "</tool_cal", "</tool_call"}
	for _, p := range partials {
		if strings.HasSuffix(content, p) {
			return true
		}
	}
	return false
}

// findPartialTagStart finds where a partial tag starts in content
func (f *GLMContentFilter) findPartialTagStart(content string) int {
	partials := []string{"<tool_call", "<tool_cal", "<tool_ca", "<tool_c", "<tool_", "<tool", "<too", "<to", "<t", "<"}
	for _, p := range partials {
		if strings.HasSuffix(content, p) {
			return len(content) - len(p)
		}
	}
	return len(content)
}

// HasToolCalls returns true if any tool calls were parsed
func (f *GLMContentFilter) HasToolCalls() bool {
	return len(f.toolCallsFound) > 0
}

// IsToolCallComplete returns true if a complete tool call was parsed in the last chunk
func (f *GLMContentFilter) IsToolCallComplete() bool {
	return f.toolCallComplete
}

// ResetToolCallComplete resets the tool call complete flag
func (f *GLMContentFilter) ResetToolCallComplete() {
	f.toolCallComplete = false
}

// GetToolCalls returns the parsed tool calls
func (f *GLMContentFilter) GetToolCalls() []GLMToolCall {
	return f.toolCallsFound
}

// IsInsideToolCall returns true if currently parsing inside a tool call tag
func (f *GLMContentFilter) IsInsideToolCall() bool {
	return f.insideToolCall
}

// FilterSSELine filters an SSE data line, modifying the content field if it contains tool call XML.
// Returns the filtered line (or original if no filtering needed) and whether it was modified.
func (f *GLMContentFilter) FilterSSELine(line string) (string, bool) {
	if !strings.HasPrefix(line, "data: ") {
		return line, false
	}

	jsonData := strings.TrimPrefix(line, "data: ")
	if jsonData == "[DONE]" {
		return line, false
	}

	// Parse JSON to extract content
	var chunk map[string]interface{}
	if err := json.Unmarshal([]byte(jsonData), &chunk); err != nil {
		return line, false
	}

	choices, ok := chunk["choices"].([]interface{})
	if !ok || len(choices) == 0 {
		return line, false
	}

	choice, ok := choices[0].(map[string]interface{})
	if !ok {
		return line, false
	}

	delta, ok := choice["delta"].(map[string]interface{})
	if !ok {
		return line, false
	}

	content, ok := delta["content"].(string)
	if !ok || content == "" {
		return line, false
	}

	// Check if content might contain tool call XML
	if !strings.Contains(content, "<tool_call") && !strings.Contains(content, "</tool_call") && !f.insideToolCall {
		// Quick check for partial tag at end
		if !f.hasPartialOpeningTag(content) {
			return line, false
		}
	}

	// Filter the content
	filteredContent := f.FilterContentChunk(content)

	// If content unchanged, return original
	if filteredContent == content {
		return line, false
	}

	// Rebuild the SSE line with filtered content
	if filteredContent == "" {
		// Entire content was tool call XML - return empty content chunk
		delta["content"] = ""
	} else {
		delta["content"] = filteredContent
	}

	newJSON, err := json.Marshal(chunk)
	if err != nil {
		return line, false
	}

	return "data: " + string(newJSON), true
}
