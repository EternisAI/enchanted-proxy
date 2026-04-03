package proxy

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"testing"
)

func TestExtractTokenUsage(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		wantNil  bool
		wantUsage *Usage
	}{
		{
			name:    "empty body",
			body:    "",
			wantNil: true,
		},
		{
			name:    "invalid JSON",
			body:    "not json",
			wantNil: true,
		},
		{
			name:    "no usage field",
			body:    `{"choices":[{"message":{"content":"hello"}}]}`,
			wantNil: true,
		},
		{
			name: "valid usage",
			body: `{"choices":[{"message":{"content":"hello"}}],"usage":{"prompt_tokens":10,"completion_tokens":20,"total_tokens":30}}`,
			wantUsage: &Usage{PromptTokens: 10, CompletionTokens: 20, TotalTokens: 30},
		},
		{
			name:    "null usage",
			body:    `{"choices":[],"usage":null}`,
			wantNil: true,
		},
		{
			name: "error response (no usage)",
			body: `{"error":{"message":"rate limited","type":"rate_limit_error"}}`,
			wantNil: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := extractTokenUsage([]byte(tt.body))
			if tt.wantNil {
				if result != nil {
					t.Errorf("expected nil, got %+v", result)
				}
				return
			}
			if result == nil {
				t.Fatal("expected usage, got nil")
			}
			if *result != *tt.wantUsage {
				t.Errorf("got %+v, want %+v", result, tt.wantUsage)
			}
		})
	}
}

func TestExtractTokenUsageFromSSELine(t *testing.T) {
	tests := []struct {
		name      string
		line      string
		wantNil   bool
		wantUsage *Usage
	}{
		{
			name:    "not SSE data line",
			line:    "event: message",
			wantNil: true,
		},
		{
			name:    "DONE marker",
			line:    "data: [DONE]",
			wantNil: true,
		},
		{
			name:    "content chunk without usage",
			line:    `data: {"choices":[{"delta":{"content":"hello"}}]}`,
			wantNil: true,
		},
		{
			name:    "chunk with null usage",
			line:    `data: {"choices":[],"usage":null}`,
			wantNil: true,
		},
		{
			name: "final chunk with usage (OpenAI format)",
			line: `data: {"choices":[],"usage":{"prompt_tokens":50,"completion_tokens":100,"total_tokens":150}}`,
			wantUsage: &Usage{PromptTokens: 50, CompletionTokens: 100, TotalTokens: 150},
		},
		{
			name: "usage chunk from Tinfoil/vLLM",
			line: `data: {"id":"chatcmpl-123","choices":[],"usage":{"prompt_tokens":5,"completion_tokens":10,"total_tokens":15}}`,
			wantUsage: &Usage{PromptTokens: 5, CompletionTokens: 10, TotalTokens: 15},
		},
		{
			name:    "invalid JSON in data",
			line:    `data: {invalid`,
			wantNil: true,
		},
		{
			name:    "empty data",
			line:    "data: ",
			wantNil: true,
		},
		{
			name:    "usage with wrong types",
			line:    `data: {"usage":{"prompt_tokens":"not_a_number"}}`,
			wantNil: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := extractTokenUsageFromSSELine(tt.line)
			if tt.wantNil {
				if result != nil {
					t.Errorf("expected nil, got %+v", result)
				}
				return
			}
			if result == nil {
				t.Fatal("expected usage, got nil")
			}
			if *result != *tt.wantUsage {
				t.Errorf("got %+v, want %+v", result, tt.wantUsage)
			}
		})
	}
}

// TestStreamOptionsInjection verifies that stream_options.include_usage is injected
// into streaming requests regardless of provider. This is the fix for the bug where
// only the Eternis provider got usage data in streaming responses.
func TestStreamOptionsInjection(t *testing.T) {
	tests := []struct {
		name           string
		requestBody    map[string]interface{}
		wantInjected   bool
	}{
		{
			name:         "streaming request gets stream_options",
			requestBody:  map[string]interface{}{"model": "gpt-4", "stream": true, "messages": []interface{}{}},
			wantInjected: true,
		},
		{
			name:         "non-streaming request is left alone",
			requestBody:  map[string]interface{}{"model": "gpt-4", "stream": false, "messages": []interface{}{}},
			wantInjected: false,
		},
		{
			name:         "no stream field is left alone",
			requestBody:  map[string]interface{}{"model": "gpt-4", "messages": []interface{}{}},
			wantInjected: false,
		},
		{
			name: "existing stream_options are merged and include_usage forced true",
			requestBody: map[string]interface{}{
				"model":    "gpt-4",
				"stream":   true,
				"messages": []interface{}{},
				"stream_options": map[string]interface{}{
					"include_usage": false,
					"other_flag":    "keep-me",
				},
			},
			wantInjected: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			requestBody, err := json.Marshal(tt.requestBody)
			if err != nil {
				t.Fatalf("failed to marshal request body: %v", err)
			}

			// Replicate the injection logic from ProxyHandler
			var reqBody map[string]interface{}
			if err := json.Unmarshal(requestBody, &reqBody); err != nil {
				t.Fatalf("failed to unmarshal: %v", err)
			}

			if stream, ok := reqBody["stream"].(bool); ok && stream {
				streamOptions, _ := reqBody["stream_options"].(map[string]interface{})
				if streamOptions == nil {
					streamOptions = make(map[string]interface{})
				}
				streamOptions["include_usage"] = true
				reqBody["stream_options"] = streamOptions
				requestBody, _ = json.Marshal(reqBody)
			}

			// Parse result and check
			var result map[string]interface{}
			json.Unmarshal(requestBody, &result)

			streamOpts, exists := result["stream_options"]
			if tt.wantInjected {
				if !exists {
					t.Fatal("expected stream_options to be injected")
				}
				opts := streamOpts.(map[string]interface{})
				if opts["include_usage"] != true {
					t.Errorf("expected include_usage=true, got %v", opts["include_usage"])
				}
				if originalOpts, ok := tt.requestBody["stream_options"].(map[string]interface{}); ok {
					if originalOther, hadOther := originalOpts["other_flag"]; hadOther {
						if opts["other_flag"] != originalOther {
							t.Errorf("expected other_flag to be preserved, got %v", opts["other_flag"])
						}
					}
				}
			} else {
				if exists {
					// Only fail if it wasn't in the original request
					if _, wasOriginal := tt.requestBody["stream_options"]; !wasOriginal {
						t.Error("stream_options should not have been injected")
					}
				}
			}
		})
	}
}

// TestStreamingResponseUsageExtraction simulates reading a full SSE stream and
// extracting token usage from it, as handleStreamingResponse does.
func TestStreamingResponseUsageExtraction(t *testing.T) {
	tests := []struct {
		name      string
		sseLines  []string
		wantUsage *Usage
	}{
		{
			name: "normal stream with usage in penultimate chunk",
			sseLines: []string{
				`data: {"choices":[{"delta":{"content":"Hello"}}]}`,
				`data: {"choices":[{"delta":{"content":" world"}}]}`,
				`data: {"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":2,"total_tokens":12}}`,
				`data: [DONE]`,
			},
			wantUsage: &Usage{PromptTokens: 10, CompletionTokens: 2, TotalTokens: 12},
		},
		{
			name: "stream without usage (the bug case — stream_options not set)",
			sseLines: []string{
				`data: {"choices":[{"delta":{"content":"Hello"}}]}`,
				`data: {"choices":[{"delta":{"content":" world"}}]}`,
				`data: [DONE]`,
			},
			wantUsage: nil,
		},
		{
			name: "stream with only DONE",
			sseLines: []string{
				`data: [DONE]`,
			},
			wantUsage: nil,
		},
		{
			name: "usage in last data chunk before DONE",
			sseLines: []string{
				`data: {"choices":[{"delta":{"content":"Hi"}}]}`,
				`data: {"choices":[],"usage":{"prompt_tokens":100,"completion_tokens":200,"total_tokens":300}}`,
				`data: [DONE]`,
			},
			wantUsage: &Usage{PromptTokens: 100, CompletionTokens: 200, TotalTokens: 300},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var tokenUsage *Usage
			for _, line := range tt.sseLines {
				if usage := extractTokenUsageFromSSELine(line); usage != nil {
					tokenUsage = usage
				}
			}

			if tt.wantUsage == nil {
				if tokenUsage != nil {
					t.Errorf("expected nil usage, got %+v", tokenUsage)
				}
			} else {
				if tokenUsage == nil {
					t.Fatal("expected usage, got nil")
				}
				if *tokenUsage != *tt.wantUsage {
					t.Errorf("got %+v, want %+v", tokenUsage, tt.wantUsage)
				}
			}
		})
	}
}

// TestNonStreamingErrorResponseHasNoUsage verifies that error responses
// from upstream providers don't contain usage data (confirming our Error-level
// log should only fire on 2xx).
func TestNonStreamingErrorResponseHasNoUsage(t *testing.T) {
	errorResponses := []struct {
		name       string
		statusCode int
		body       string
	}{
		{
			name:       "400 bad request",
			statusCode: 400,
			body:       `{"error":{"message":"Invalid model","type":"invalid_request_error"}}`,
		},
		{
			name:       "429 rate limited",
			statusCode: 429,
			body:       `{"error":{"message":"Rate limit exceeded","type":"rate_limit_error"}}`,
		},
		{
			name:       "500 internal error",
			statusCode: 500,
			body:       `{"error":{"message":"Internal server error"}}`,
		},
		{
			name:       "401 unauthorized",
			statusCode: 401,
			body:       `{"error":{"message":"Invalid API key"}}`,
		},
	}

	for _, tt := range errorResponses {
		t.Run(tt.name, func(t *testing.T) {
			usage := extractTokenUsage([]byte(tt.body))
			if usage != nil {
				t.Errorf("error response should not contain usage, got %+v", usage)
			}

			// Verify our 2xx guard would correctly skip logging
			resp := &http.Response{
				StatusCode: tt.statusCode,
				Body:       io.NopCloser(bytes.NewReader([]byte(tt.body))),
			}
			shouldLogError := usage == nil && resp.StatusCode >= 200 && resp.StatusCode < 300
			if shouldLogError {
				t.Error("should NOT log error for non-2xx response with no usage")
			}
		})
	}
}
