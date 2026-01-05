package streaming

import (
	"encoding/json"
	"testing"
)

func TestGLMContentFilter_BasicToolCall(t *testing.T) {
	filter := NewGLMContentFilter()

	content := `I'll search for that.<tool_call>web_search
<arg_key>query</arg_key>
<arg_value>Zhipu AI IPO</arg_value>
</tool_call>`

	result := filter.FilterContentChunk(content)

	if result != "I'll search for that." {
		t.Errorf("expected filtered content 'I'll search for that.', got '%s'", result)
	}

	if !filter.HasToolCalls() {
		t.Error("expected tool calls to be detected")
	}

	toolCalls := filter.GetToolCalls()
	if len(toolCalls) != 1 {
		t.Errorf("expected 1 tool call, got %d", len(toolCalls))
	}

	if toolCalls[0].Name != "web_search" {
		t.Errorf("expected tool name 'web_search', got '%s'", toolCalls[0].Name)
	}

	if toolCalls[0].Arguments["query"] != "Zhipu AI IPO" {
		t.Errorf("expected query arg 'Zhipu AI IPO', got '%s'", toolCalls[0].Arguments["query"])
	}
}

func TestGLMContentFilter_DuplicatedOpenTags(t *testing.T) {
	filter := NewGLMContentFilter()

	content := `I'll search for that.<tool_call><tool_call><tool_call>web_search
<arg_key>query</arg_key>
<arg_value>test query</arg_value>
</tool_call>`

	result := filter.FilterContentChunk(content)

	if result != "I'll search for that." {
		t.Errorf("expected filtered content 'I'll search for that.', got '%s'", result)
	}

	if !filter.HasToolCalls() {
		t.Error("expected tool calls to be detected")
	}

	toolCalls := filter.GetToolCalls()
	if len(toolCalls) != 1 {
		t.Errorf("expected 1 tool call, got %d", len(toolCalls))
	}

	if toolCalls[0].Name != "web_search" {
		t.Errorf("expected tool name 'web_search', got '%s'", toolCalls[0].Name)
	}
}

func TestGLMContentFilter_StreamingChunks(t *testing.T) {
	filter := NewGLMContentFilter()

	chunks := []string{
		"I'll search for information about",
		" Zhipu AI.<tool_call>web",
		"_search\n<arg_key>query</arg_key>\n<arg_value>Zhipu AI</arg_value>\n</tool_call>",
		" Here are the results.",
	}

	var totalOutput string
	for _, chunk := range chunks {
		result := filter.FilterContentChunk(chunk)
		totalOutput += result
	}

	expected := "I'll search for information about Zhipu AI. Here are the results."
	if totalOutput != expected {
		t.Errorf("expected '%s', got '%s'", expected, totalOutput)
	}

	if !filter.HasToolCalls() {
		t.Error("expected tool calls to be detected")
	}
}

func TestGLMContentFilter_PartialTagAtBoundary(t *testing.T) {
	filter := NewGLMContentFilter()

	chunk1 := "Searching now<tool_ca"
	chunk2 := "ll>web_search\n</tool_call>"

	result1 := filter.FilterContentChunk(chunk1)
	result2 := filter.FilterContentChunk(chunk2)

	if result1 != "Searching now" {
		t.Errorf("expected 'Searching now', got '%s'", result1)
	}

	if result2 != "" {
		t.Errorf("expected empty string, got '%s'", result2)
	}

	if !filter.HasToolCalls() {
		t.Error("expected tool calls to be detected")
	}
}

func TestGLMContentFilter_NoToolCall(t *testing.T) {
	filter := NewGLMContentFilter()

	content := "This is regular content without any tool calls."
	result := filter.FilterContentChunk(content)

	if result != content {
		t.Errorf("expected unchanged content, got '%s'", result)
	}

	if filter.HasToolCalls() {
		t.Error("expected no tool calls")
	}
}

func TestGLMContentFilter_MultipleArguments(t *testing.T) {
	filter := NewGLMContentFilter()

	content := `<tool_call>search
<arg_key>query</arg_key>
<arg_value>AI news</arg_value>
<arg_key>limit</arg_key>
<arg_value>10</arg_value>
</tool_call>`

	result := filter.FilterContentChunk(content)

	if result != "" {
		t.Errorf("expected empty result, got '%s'", result)
	}

	toolCalls := filter.GetToolCalls()
	if len(toolCalls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(toolCalls))
	}

	if toolCalls[0].Arguments["query"] != "AI news" {
		t.Errorf("expected query 'AI news', got '%s'", toolCalls[0].Arguments["query"])
	}

	if toolCalls[0].Arguments["limit"] != "10" {
		t.Errorf("expected limit '10', got '%s'", toolCalls[0].Arguments["limit"])
	}
}

func TestGLMContentFilter_FilterSSELine(t *testing.T) {
	filter := NewGLMContentFilter()

	sseData := map[string]interface{}{
		"choices": []interface{}{
			map[string]interface{}{
				"delta": map[string]interface{}{
					"content": "Hello<tool_call>web_search\n<arg_key>q</arg_key>\n<arg_value>test</arg_value>\n</tool_call>",
				},
			},
		},
	}
	jsonBytes, _ := json.Marshal(sseData)
	line := "data: " + string(jsonBytes)

	filteredLine, wasFiltered := filter.FilterSSELine(line)

	if !wasFiltered {
		t.Error("expected line to be filtered")
	}

	var result map[string]interface{}
	json.Unmarshal([]byte(filteredLine[6:]), &result) // Skip "data: "

	choices := result["choices"].([]interface{})
	delta := choices[0].(map[string]interface{})["delta"].(map[string]interface{})
	content := delta["content"].(string)

	if content != "Hello" {
		t.Errorf("expected content 'Hello', got '%s'", content)
	}
}

func TestGLMContentFilter_FilterSSELine_NoToolCall(t *testing.T) {
	filter := NewGLMContentFilter()

	sseData := map[string]interface{}{
		"choices": []interface{}{
			map[string]interface{}{
				"delta": map[string]interface{}{
					"content": "Regular content without tool calls",
				},
			},
		},
	}
	jsonBytes, _ := json.Marshal(sseData)
	line := "data: " + string(jsonBytes)

	filteredLine, wasFiltered := filter.FilterSSELine(line)

	if wasFiltered {
		t.Error("expected line to NOT be filtered")
	}

	if filteredLine != line {
		t.Error("expected line to be unchanged")
	}
}

func TestGLMContentFilter_MalformedToolCallInFunctionName(t *testing.T) {
	filter := NewGLMContentFilter()

	content := `<tool_call><tool_call>Read</tool_call>`

	result := filter.FilterContentChunk(content)

	if result != "" {
		t.Errorf("expected empty result, got '%s'", result)
	}

	if !filter.HasToolCalls() {
		t.Error("expected tool calls to be detected")
	}

	toolCalls := filter.GetToolCalls()
	if len(toolCalls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(toolCalls))
	}

	if toolCalls[0].Name != "Read" {
		t.Errorf("expected tool name 'Read', got '%s'", toolCalls[0].Name)
	}
}

func TestGLMContentFilter_ExactBugScenario(t *testing.T) {
	filter := NewGLMContentFilter()

	content := `I'll search for the latest information on Zhipu Al's IPO plans, their growth potential compared to top Al companies, and the comparison between US and China tech IPOs.<tool_call><tool_call>web<tool_call>web_s`

	result := filter.FilterContentChunk(content)

	expected := "I'll search for the latest information on Zhipu Al's IPO plans, their growth potential compared to top Al companies, and the comparison between US and China tech IPOs."
	if result != expected {
		t.Errorf("expected '%s', got '%s'", expected, result)
	}

	if !filter.IsInsideToolCall() {
		t.Error("expected to be inside tool call (incomplete)")
	}

	continuation := `earch
<arg_key>query</arg_key>
<arg_value>Zhipu AI IPO</arg_value>
</tool_call> Here is what I found.`

	result2 := filter.FilterContentChunk(continuation)

	if result2 != " Here is what I found." {
		t.Errorf("expected ' Here is what I found.', got '%s'", result2)
	}

	if filter.IsInsideToolCall() {
		t.Error("expected to NOT be inside tool call after closing")
	}
}
