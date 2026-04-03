package probe

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestParseResponse(t *testing.T) {
	tests := []struct {
		name            string
		body            string
		wantContent     string
		wantUsageNil    bool
		wantPromptToks  int
		wantCompleteTok int
	}{
		{
			name: "full response with usage",
			body: `{
				"choices": [{"message": {"content": "pong"}}],
				"usage": {"prompt_tokens": 5, "completion_tokens": 1}
			}`,
			wantContent:     "pong",
			wantPromptToks:  5,
			wantCompleteTok: 1,
		},
		{
			name: "response without usage",
			body: `{
				"choices": [{"message": {"content": "hello"}}]
			}`,
			wantContent:  "hello",
			wantUsageNil: true,
		},
		{
			name:         "empty choices",
			body:         `{"choices": []}`,
			wantContent:  "",
			wantUsageNil: true,
		},
		{
			name:         "invalid JSON",
			body:         `not json`,
			wantContent:  "",
			wantUsageNil: true,
		},
		{
			name:         "empty body",
			body:         ``,
			wantContent:  "",
			wantUsageNil: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := parseResponse([]byte(tt.body))

			if result.content != tt.wantContent {
				t.Errorf("content = %q, want %q", result.content, tt.wantContent)
			}

			if tt.wantUsageNil {
				if result.usage != nil {
					t.Errorf("expected nil usage, got %+v", result.usage)
				}
			} else {
				if result.usage == nil {
					t.Fatal("expected non-nil usage")
				}
				if result.usage.PromptTokens != tt.wantPromptToks {
					t.Errorf("prompt_tokens = %d, want %d", result.usage.PromptTokens, tt.wantPromptToks)
				}
				if result.usage.CompletionTokens != tt.wantCompleteTok {
					t.Errorf("completion_tokens = %d, want %d", result.usage.CompletionTokens, tt.wantCompleteTok)
				}
			}
		})
	}
}

func TestParseResponsesAPIResponse(t *testing.T) {
	tests := []struct {
		name            string
		body            string
		wantContent     string
		wantUsageNil    bool
		wantPromptToks  int
		wantCompleteTok int
	}{
		{
			name: "full response with usage",
			body: `{
				"id": "resp_abc123",
				"status": "completed",
				"output": [
					{
						"type": "message",
						"role": "assistant",
						"content": [
							{"type": "output_text", "text": "OK"}
						]
					}
				],
				"usage": {"input_tokens": 10, "output_tokens": 1}
			}`,
			wantContent:     "OK",
			wantPromptToks:  10,
			wantCompleteTok: 1,
		},
		{
			name: "response without usage",
			body: `{
				"id": "resp_abc123",
				"status": "completed",
				"output": [
					{
						"type": "message",
						"role": "assistant",
						"content": [
							{"type": "output_text", "text": "hello"}
						]
					}
				]
			}`,
			wantContent:  "hello",
			wantUsageNil: true,
		},
		{
			name: "multiple output items — picks first message",
			body: `{
				"status": "completed",
				"output": [
					{
						"type": "reasoning",
						"content": [{"type": "reasoning_text", "text": "thinking..."}]
					},
					{
						"type": "message",
						"role": "assistant",
						"content": [
							{"type": "output_text", "text": "answer"}
						]
					}
				]
			}`,
			wantContent:  "answer",
			wantUsageNil: true,
		},
		{
			name:         "empty output",
			body:         `{"status": "completed", "output": []}`,
			wantContent:  "",
			wantUsageNil: true,
		},
		{
			name:         "invalid JSON",
			body:         `not json`,
			wantContent:  "",
			wantUsageNil: true,
		},
		{
			name:         "empty body",
			body:         ``,
			wantContent:  "",
			wantUsageNil: true,
		},
		{
			name: "message with no output_text content",
			body: `{
				"status": "completed",
				"output": [
					{
						"type": "message",
						"role": "assistant",
						"content": [
							{"type": "refusal", "text": "I cannot help with that"}
						]
					}
				]
			}`,
			wantContent:  "",
			wantUsageNil: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := parseResponsesAPIResponse([]byte(tt.body))

			if result.content != tt.wantContent {
				t.Errorf("content = %q, want %q", result.content, tt.wantContent)
			}

			if tt.wantUsageNil {
				if result.usage != nil {
					t.Errorf("expected nil usage, got %+v", result.usage)
				}
			} else {
				if result.usage == nil {
					t.Fatal("expected non-nil usage")
				}
				if result.usage.PromptTokens != tt.wantPromptToks {
					t.Errorf("prompt_tokens = %d, want %d", result.usage.PromptTokens, tt.wantPromptToks)
				}
				if result.usage.CompletionTokens != tt.wantCompleteTok {
					t.Errorf("completion_tokens = %d, want %d", result.usage.CompletionTokens, tt.wantCompleteTok)
				}
			}
		})
	}
}

func TestParseResponse_RoundTrip(t *testing.T) {
	// Verify parseResponse handles a realistically shaped response.
	resp := map[string]any{
		"id":      "chatcmpl-abc",
		"object":  "chat.completion",
		"created": 1700000000,
		"choices": []map[string]any{
			{
				"index":         0,
				"message":       map[string]string{"role": "assistant", "content": "pong"},
				"finish_reason": "stop",
			},
		},
		"usage": map[string]int{
			"prompt_tokens":     8,
			"completion_tokens": 1,
			"total_tokens":      9,
		},
	}

	body, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	result := parseResponse(body)
	if result.content != "pong" {
		t.Errorf("content = %q, want %q", result.content, "pong")
	}
	if result.usage == nil {
		t.Fatal("expected non-nil usage")
	}
	if result.usage.PromptTokens != 8 || result.usage.CompletionTokens != 1 {
		t.Errorf("usage = %+v, want {8, 1}", result.usage)
	}
}

func TestTruncate(t *testing.T) {
	tests := []struct {
		name   string
		input  string
		maxLen int
		want   string
		exact  bool // if false, just check prefix + that it contains "total"
	}{
		{
			name:   "short string unchanged",
			input:  "hello",
			maxLen: 10,
			want:   "hello",
			exact:  true,
		},
		{
			name:   "exact length unchanged",
			input:  "hello",
			maxLen: 5,
			want:   "hello",
			exact:  true,
		},
		{
			name:   "truncated with suffix",
			input:  "hello world",
			maxLen: 5,
			want:   "hello",
			exact:  false,
		},
		{
			name:   "empty string",
			input:  "",
			maxLen: 5,
			want:   "",
			exact:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := truncate(tt.input, tt.maxLen)
			if tt.exact {
				if got != tt.want {
					t.Errorf("truncate(%q, %d) = %q, want %q", tt.input, tt.maxLen, got, tt.want)
				}
			} else {
				if len(got) <= tt.maxLen {
					t.Errorf("expected truncated string to be longer than %d (has suffix), got %q", tt.maxLen, got)
				}
				runes := []rune(got)
				prefix := string(runes[:len([]rune(tt.want))])
				if prefix != tt.want {
					t.Errorf("truncated prefix = %q, want %q", prefix, tt.want)
				}
				if !strings.Contains(got, "total") {
					t.Errorf("expected truncated result to contain %q, got %q", "total", got)
				}
			}
		})
	}
}
