package streaming

import (
	"testing"
)

func TestToolCallDetector_BasicFlow(t *testing.T) {
	d := NewToolCallDetector()

	// First chunk: tool call ID and name
	chunk1 := `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_123","type":"function","function":{"name":"web_search","arguments":""}}]}}]}`
	if !d.ProcessChunk(chunk1) {
		t.Fatal("expected chunk1 to be detected as tool call")
	}

	// Middle chunk: arguments
	chunk2 := `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"queries\":[\"weather today\"]}"}}]}}]}`
	if !d.ProcessChunk(chunk2) {
		t.Fatal("expected chunk2 to be detected as tool call")
	}

	// Final chunk: finish_reason
	chunk3 := `data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`
	d.ProcessChunk(chunk3)

	if !d.IsComplete() {
		t.Fatal("expected tool calls to be complete")
	}

	toolCalls := d.GetToolCalls()
	if len(toolCalls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(toolCalls))
	}
	if toolCalls[0].Function.Arguments != `{"queries":["weather today"]}` {
		t.Fatalf("unexpected arguments: %s", toolCalls[0].Function.Arguments)
	}
}

func TestToolCallDetector_ArgumentsInFinishReasonChunk(t *testing.T) {
	// Regression test: some providers (e.g., Kimi/Moonshot via Tinfoil) send
	// tool_calls data in the same chunk as finish_reason="tool_calls".
	// Previously, the early return on finish_reason would skip argument processing.
	d := NewToolCallDetector()

	// First chunk: tool call ID and name (no arguments yet)
	chunk1 := `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_abc","type":"function","function":{"name":"web_search","arguments":""}}]}}]}`
	d.ProcessChunk(chunk1)

	// Second chunk: finish_reason AND final arguments in same chunk
	chunk2 := `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"queries\":[\"current weather\"]}"}}]},"finish_reason":"tool_calls"}]}`
	d.ProcessChunk(chunk2)

	if !d.IsComplete() {
		t.Fatal("expected tool calls to be complete")
	}

	toolCalls := d.GetToolCalls()
	if len(toolCalls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(toolCalls))
	}
	if toolCalls[0].Function.Name != "web_search" {
		t.Fatalf("unexpected name: %s", toolCalls[0].Function.Name)
	}
	if toolCalls[0].Function.Arguments != `{"queries":["current weather"]}` {
		t.Fatalf("unexpected arguments: %q", toolCalls[0].Function.Arguments)
	}
}

func TestToolCallDetector_AllInOneChunk(t *testing.T) {
	// Some providers send the complete tool call in a single chunk
	d := NewToolCallDetector()

	chunk := `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_xyz","type":"function","function":{"name":"web_search","arguments":"{\"queries\":[\"hello\"]}"}}]},"finish_reason":"tool_calls"}]}`
	d.ProcessChunk(chunk)

	if !d.IsComplete() {
		t.Fatal("expected tool calls to be complete")
	}

	toolCalls := d.GetToolCalls()
	if len(toolCalls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(toolCalls))
	}
	if toolCalls[0].Function.Arguments != `{"queries":["hello"]}` {
		t.Fatalf("unexpected arguments: %q", toolCalls[0].Function.Arguments)
	}
}

func TestToolCallDetector_EmptyArgsWithoutFinishChunk(t *testing.T) {
	// If no arguments are ever sent, arguments should be empty
	d := NewToolCallDetector()

	chunk1 := `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"web_search","arguments":""}}]}}]}`
	d.ProcessChunk(chunk1)

	chunk2 := `data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`
	d.ProcessChunk(chunk2)

	if !d.IsComplete() {
		t.Fatal("expected complete")
	}

	toolCalls := d.GetToolCalls()
	if len(toolCalls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(toolCalls))
	}
	// Arguments should be empty string (would fail JSON parse, but that's handled by executor)
	if toolCalls[0].Function.Arguments != "" {
		t.Fatalf("expected empty arguments, got: %q", toolCalls[0].Function.Arguments)
	}
}
