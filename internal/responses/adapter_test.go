package responses

import (
	"encoding/json"
	"testing"
)

func TestAdapter_TransformRequest(t *testing.T) {
	adapter := NewAdapter()

	tests := []struct {
		name               string
		requestBody        string
		previousResponseID string
		wantStoreField     bool
		wantPreviousRespID bool
		wantErr            bool
	}{
		{
			name: "first message - adds store=true",
			requestBody: `{
				"model": "gpt-5-pro",
				"messages": [{"role": "user", "content": "Hello"}]
			}`,
			previousResponseID: "",
			wantStoreField:     true,
			wantPreviousRespID: false,
			wantErr:            false,
		},
		{
			name: "continuation - adds store=true and previous_response_id",
			requestBody: `{
				"model": "gpt-5-pro",
				"messages": [{"role": "user", "content": "Tell me more"}]
			}`,
			previousResponseID: "resp_abc123",
			wantStoreField:     true,
			wantPreviousRespID: true,
			wantErr:            false,
		},
		{
			name: "preserves existing fields",
			requestBody: `{
				"model": "gpt-5-pro",
				"messages": [{"role": "user", "content": "Hello"}],
				"temperature": 0.7,
				"max_tokens": 1000
			}`,
			previousResponseID: "",
			wantStoreField:     true,
			wantPreviousRespID: false,
			wantErr:            false,
		},
		{
			name:               "invalid JSON returns error",
			requestBody:        `{"invalid json`,
			previousResponseID: "",
			wantErr:            true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			transformed, err := adapter.TransformRequest([]byte(tt.requestBody), tt.previousResponseID)

			if tt.wantErr {
				if err == nil {
					t.Errorf("TransformRequest() expected error, got nil")
				}
				return
			}

			if err != nil {
				t.Errorf("TransformRequest() unexpected error: %v", err)
				return
			}

			// Parse transformed request
			var result map[string]interface{}
			if err := json.Unmarshal(transformed, &result); err != nil {
				t.Fatalf("Failed to parse transformed request: %v", err)
			}

			// Check store field
			if tt.wantStoreField {
				store, ok := result["store"].(bool)
				if !ok || !store {
					t.Errorf("TransformRequest() store field missing or not true, got: %v", result["store"])
				}
			}

			// Check previous_response_id field
			if tt.wantPreviousRespID {
				prevID, ok := result["previous_response_id"].(string)
				if !ok {
					t.Errorf("TransformRequest() previous_response_id missing")
				} else if prevID != tt.previousResponseID {
					t.Errorf("TransformRequest() previous_response_id = %v, want %v", prevID, tt.previousResponseID)
				}
			} else {
				if _, exists := result["previous_response_id"]; exists {
					t.Errorf("TransformRequest() unexpected previous_response_id field")
				}
			}

			// Verify original fields preserved
			if tt.name == "preserves existing fields" {
				if temp, ok := result["temperature"].(float64); !ok || temp != 0.7 {
					t.Errorf("TransformRequest() temperature not preserved")
				}
				if maxTokens, ok := result["max_tokens"].(float64); !ok || maxTokens != 1000 {
					t.Errorf("TransformRequest() max_tokens not preserved")
				}
			}
		})
	}
}

func TestAdapter_ExtractResponseID(t *testing.T) {
	adapter := NewAdapter()

	tests := []struct {
		name     string
		sseChunk string
		want     string
	}{
		{
			name:     "valid response_id",
			sseChunk: `data: {"id":"resp_abc123","object":"response","created":1234567890}`,
			want:     "resp_abc123",
		},
		{
			name:     "valid response_id with content",
			sseChunk: `data: {"id":"resp_xyz456","object":"response","choices":[{"delta":{"content":"Hello"}}]}`,
			want:     "resp_xyz456",
		},
		{
			name:     "chat completion id (not resp_ prefix)",
			sseChunk: `data: {"id":"chatcmpl-123","object":"chat.completion.chunk","created":1234567890}`,
			want:     "",
		},
		{
			name:     "done message",
			sseChunk: `data: [DONE]`,
			want:     "",
		},
		{
			name:     "invalid json",
			sseChunk: `data: {invalid`,
			want:     "",
		},
		{
			name:     "missing data prefix",
			sseChunk: `{"id":"resp_abc123"}`,
			want:     "",
		},
		{
			name:     "empty string",
			sseChunk: ``,
			want:     "",
		},
		{
			name:     "no id field",
			sseChunk: `data: {"object":"response","created":1234567890}`,
			want:     "",
		},
		{
			name:     "id is not string",
			sseChunk: `data: {"id":12345,"object":"response"}`,
			want:     "",
		},
		{
			name:     "short resp_ prefix",
			sseChunk: `data: {"id":"resp_"}`,
			want:     "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := adapter.ExtractResponseID(tt.sseChunk)
			if got != tt.want {
				t.Errorf("ExtractResponseID() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestAdapter_TransformResponseChunk(t *testing.T) {
	adapter := NewAdapter()

	tests := []struct {
		name    string
		chunk   string
		want    string
		wantErr bool
	}{
		{
			name:    "regular data chunk passes through",
			chunk:   `data: {"id":"resp_123","choices":[{"delta":{"content":"Hello"}}]}`,
			want:    `data: {"id":"resp_123","choices":[{"delta":{"content":"Hello"}}]}`,
			wantErr: false,
		},
		{
			name:    "done message passes through",
			chunk:   `data: [DONE]`,
			want:    `data: [DONE]`,
			wantErr: false,
		},
		{
			name:    "empty chunk passes through",
			chunk:   ``,
			want:    ``,
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := adapter.TransformResponseChunk(tt.chunk)
			if (err != nil) != tt.wantErr {
				t.Errorf("TransformResponseChunk() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if got != tt.want {
				t.Errorf("TransformResponseChunk() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestAdapter_IsResponsesAPIError(t *testing.T) {
	adapter := NewAdapter()

	tests := []struct {
		name     string
		sseChunk string
		want     bool
	}{
		{
			name:     "error chunk",
			sseChunk: `data: {"error":{"message":"Rate limit exceeded","type":"rate_limit_error","code":"rate_limit"}}`,
			want:     true,
		},
		{
			name:     "regular data chunk",
			sseChunk: `data: {"id":"resp_123","choices":[{"delta":{"content":"Hello"}}]}`,
			want:     false,
		},
		{
			name:     "done message",
			sseChunk: `data: [DONE]`,
			want:     false,
		},
		{
			name:     "invalid json",
			sseChunk: `data: {invalid`,
			want:     false,
		},
		{
			name:     "missing data prefix",
			sseChunk: `{"error":{"message":"Error"}}`,
			want:     false,
		},
		{
			name:     "empty string",
			sseChunk: ``,
			want:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := adapter.IsResponsesAPIError(tt.sseChunk)
			if got != tt.want {
				t.Errorf("IsResponsesAPIError() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestAdapter_TransformRequest_ConcurrentSafety(t *testing.T) {
	// Test that the adapter is safe for concurrent use
	adapter := NewAdapter()
	requestBody := `{"model":"gpt-5-pro","messages":[{"role":"user","content":"Hello"}]}`

	done := make(chan bool)
	for i := 0; i < 10; i++ {
		go func() {
			_, err := adapter.TransformRequest([]byte(requestBody), "")
			if err != nil {
				t.Errorf("Concurrent TransformRequest() error: %v", err)
			}
			done <- true
		}()
	}

	for i := 0; i < 10; i++ {
		<-done
	}
}

func TestAdapter_ExtractResponseID_ConcurrentSafety(t *testing.T) {
	// Test that the adapter is safe for concurrent use
	adapter := NewAdapter()
	chunk := `data: {"id":"resp_abc123","object":"response"}`

	done := make(chan bool)
	for i := 0; i < 10; i++ {
		go func() {
			responseID := adapter.ExtractResponseID(chunk)
			if responseID != "resp_abc123" {
				t.Errorf("Concurrent ExtractResponseID() = %v, want resp_abc123", responseID)
			}
			done <- true
		}()
	}

	for i := 0; i < 10; i++ {
		<-done
	}
}
