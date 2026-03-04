package streaming

import (
	"encoding/json"
	"testing"
)

func TestNormalizeSSEErrorLine(t *testing.T) {
	tests := []struct {
		name        string
		input       string
		wantChanged bool
		wantContent string // expected content delta substring (if changed)
	}{
		{
			name:        "plain text error from provider",
			input:       "data: error: Failed to perform completion: error decoding response body",
			wantChanged: true,
			wantContent: "error: Failed to perform completion: error decoding response body",
		},
		{
			name:        "internal server error",
			input:       "data: Internal Server Error",
			wantChanged: true,
			wantContent: "Internal Server Error",
		},
		{
			name:        "normal JSON chunk passes through",
			input:       `data: {"id":"chatcmpl-1","object":"chat.completion.chunk","choices":[{"delta":{"content":"hi"}}]}`,
			wantChanged: false,
		},
		{
			name:        "DONE marker passes through",
			input:       "data: [DONE]",
			wantChanged: false,
		},
		{
			name:        "non-data line passes through",
			input:       "event: message",
			wantChanged: false,
		},
		{
			name:        "empty line passes through",
			input:       "",
			wantChanged: false,
		},
		{
			name:        "data prefix only",
			input:       "data: ",
			wantChanged: true,
			wantContent: "",
		},
		{
			name:        "arbitrary non-JSON text",
			input:       "data: some random upstream garbage",
			wantChanged: true,
			wantContent: "some random upstream garbage",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, changed := NormalizeSSEErrorLine(tt.input)

			if changed != tt.wantChanged {
				t.Errorf("changed = %v, want %v", changed, tt.wantChanged)
			}

			if !changed {
				if result != tt.input {
					t.Errorf("unchanged line was modified: got %q, want %q", result, tt.input)
				}
				return
			}

			// Verify the result is valid SSE with parseable JSON
			if len(result) < 6 || result[:6] != "data: " {
				t.Fatalf("result missing 'data: ' prefix: %q", result)
			}

			jsonStr := result[6:]
			var chunk struct {
				ID      string `json:"id"`
				Object  string `json:"object"`
				Choices []struct {
					Index        int `json:"index"`
					Delta        struct {
						Content string `json:"content"`
					} `json:"delta"`
					FinishReason string `json:"finish_reason"`
				} `json:"choices"`
			}

			if err := json.Unmarshal([]byte(jsonStr), &chunk); err != nil {
				t.Fatalf("result is not valid JSON: %v\nraw: %s", err, jsonStr)
			}

			if chunk.Object != "chat.completion.chunk" {
				t.Errorf("object = %q, want %q", chunk.Object, "chat.completion.chunk")
			}

			if len(chunk.Choices) != 1 {
				t.Fatalf("expected 1 choice, got %d", len(chunk.Choices))
			}

			choice := chunk.Choices[0]
			if choice.FinishReason != "stop" {
				t.Errorf("finish_reason = %q, want %q", choice.FinishReason, "stop")
			}

			expectedContent := "\n\n[Stream error: " + tt.wantContent + "]"
			if choice.Delta.Content != expectedContent {
				t.Errorf("content = %q, want %q", choice.Delta.Content, expectedContent)
			}
		})
	}
}
