package responses

import (
	"encoding/json"
	"fmt"
)

// Adapter handles transformation between Chat Completions API format and Responses API format.
//
// Purpose:
//   - Clients send requests in Chat Completions format (familiar API)
//   - OpenAI Responses API expects different format (stateful, response_id based)
//   - This adapter translates between the two formats transparently
//
// Key differences:
//
//	Chat Completions:     Responses API:
//	- Stateless           - Stateful (server-side state)
//	- /chat/completions   - /responses
//	- Full message array  - Optional previous_response_id
//	- No store param      - store: true for persistence
//
// Thread-safety: All methods are stateless and thread-safe.
type Adapter struct{}

// NewAdapter creates a new Responses API adapter.
func NewAdapter() *Adapter {
	return &Adapter{}
}

// TransformRequest converts a Chat Completions API request to Responses API format.
//
// Parameters:
//   - requestBody: Original request body from client (Chat Completions format)
//   - previousResponseID: Optional previous response ID for conversation continuation (empty for first message)
//
// Returns:
//   - []byte: Transformed request body for Responses API
//   - error: If transformation failed
//
// Transformations applied:
//  1. Rename "messages" to "input" (Responses API requirement)
//  2. Transform "reasoning_effort" to "reasoning.effort" (Responses API requirement)
//  3. Add "store": true to enable server-side state persistence
//  4. Add "background": true to enable polling mode (avoids timeout issues)
//  5. Add "previous_response_id" if continuing conversation
//  6. Set "reasoning.effort" to "high" (default for GPT-5 Pro, if not provided)
//  7. Keep all other parameters (model, temperature, etc.)
//
// Example:
//
//	Input (Chat Completions):
//	  {"model": "gpt-5-pro", "messages": [{"role": "user", "content": "Hello"}]}
//
//	Output (Responses API, first message):
//	  {"model": "gpt-5-pro", "input": [{"role": "user", "content": "Hello"}],
//	   "store": true, "background": true, "reasoning": {"effort": "high"}}
//
//	Output (Responses API, continuation):
//	  {"model": "gpt-5-pro", "input": [{"role": "user", "content": "Tell me more"}],
//	   "store": true, "background": true, "previous_response_id": "resp_abc123",
//	   "reasoning": {"effort": "high"}}
func (a *Adapter) TransformRequest(requestBody []byte, previousResponseID string) ([]byte, error) {
	// Parse original request
	var req map[string]interface{}
	if err := json.Unmarshal(requestBody, &req); err != nil {
		return nil, fmt.Errorf("failed to parse request body: %w", err)
	}

	// Responses API uses "input" instead of "messages"
	// Move messages array to input
	if messages, exists := req["messages"]; exists {
		req["input"] = messages
		delete(req, "messages")
	}

	// Transform "reasoning_effort" to "reasoning.effort"
	// Client might send reasoning_effort as top-level parameter (Chat Completions style)
	// Responses API expects it nested under reasoning.effort
	if reasoningEffort, exists := req["reasoning_effort"]; exists {
		req["reasoning"] = map[string]interface{}{
			"effort": reasoningEffort,
		}
		delete(req, "reasoning_effort")
	}

	// Enable stateful conversation
	req["store"] = true

	// Enable background mode (polling instead of streaming)
	// This avoids timeout issues for long-running GPT-5 Pro requests
	req["background"] = true

	// Add previous_response_id if continuing conversation
	if previousResponseID != "" {
		req["previous_response_id"] = previousResponseID
	}

	// Set reasoning effort to "high" (default for GPT-5 Pro)
	// Only set default if client hasn't provided reasoning parameter
	if _, exists := req["reasoning"]; !exists {
		req["reasoning"] = map[string]interface{}{
			"effort": "high",
		}
	}

	// Marshal back to JSON
	transformed, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal transformed request: %w", err)
	}

	return transformed, nil
}

// ExtractResponseID extracts the response ID from a Responses API SSE chunk.
//
// Parameters:
//   - sseChunk: A single SSE line from the Responses API stream
//
// Returns:
//   - string: The response ID if found (e.g., "resp_abc123"), or empty string if not present
//
// The Responses API includes the response ID in the first chunk:
//
//	data: {"id":"resp_abc123","object":"response","created":1234567890,...}
//
// This ID is needed for:
//  1. Continuing conversations (previous_response_id parameter)
//  2. Canceling responses (DELETE /responses/{responseId})
//  3. Tracking conversation state across devices
//
// Thread-safe: Pure function, no shared state.
func (a *Adapter) ExtractResponseID(sseChunk string) string {
	// SSE format: "data: {json}"
	if len(sseChunk) < 6 || sseChunk[:6] != "data: " {
		return ""
	}

	// Extract JSON portion
	jsonStr := sseChunk[6:]
	if jsonStr == "[DONE]" {
		return ""
	}

	// Parse JSON
	var chunk map[string]interface{}
	if err := json.Unmarshal([]byte(jsonStr), &chunk); err != nil {
		return ""
	}

	// Extract ID field
	if id, ok := chunk["id"].(string); ok {
		// Responses API uses "resp_" prefix
		if len(id) > 5 && id[:5] == "resp_" {
			return id
		}
	}

	return ""
}

// TransformResponseChunk converts a Responses API SSE chunk to Chat Completions format.
//
// Parameters:
//   - responsesChunk: SSE chunk from Responses API
//
// Returns:
//   - string: Transformed chunk in Chat Completions SSE format
//   - error: If transformation failed
//
// Why transform?
//   - Clients expect Chat Completions format (choices[].delta.content)
//   - Responses API uses different format (choices[].message.content or delta structure)
//   - We translate so clients don't need to know about Responses API
//
// Transformation:
//
//	Responses API format varies - sometimes uses "message", sometimes "delta"
//	We normalize to the familiar Chat Completions delta format:
//	  data: {"choices":[{"delta":{"content":"Hello"}}]}
//
// Special cases:
//   - [DONE] token: Pass through unchanged
//   - Metadata chunks (id, model): Pass through (useful for response_id extraction)
//   - Error chunks: Pass through unchanged
//
// Thread-safe: Pure function, no shared state.
func (a *Adapter) TransformResponseChunk(responsesChunk string) (string, error) {
	// Pass through special tokens unchanged
	if responsesChunk == "data: [DONE]" || responsesChunk == "" {
		return responsesChunk, nil
	}

	// For now, pass through all chunks unchanged
	// The Responses API SSE format is similar enough to Chat Completions
	// that most clients can parse it directly
	//
	// TODO: If we encounter format incompatibilities, add transformation here
	// Example transformation:
	//   - Extract "message.content" and convert to "delta.content"
	//   - Normalize field names
	//   - Filter out Responses-specific fields

	return responsesChunk, nil
}

// IsResponsesAPIError checks if an SSE chunk contains a Responses API specific error.
//
// Parameters:
//   - sseChunk: SSE line to check
//
// Returns:
//   - bool: true if this is an error chunk
//
// Responses API error format:
//
//	data: {"error":{"message":"...", "type":"...", "code":"..."}}
func (a *Adapter) IsResponsesAPIError(sseChunk string) bool {
	if len(sseChunk) < 6 || sseChunk[:6] != "data: " {
		return false
	}

	jsonStr := sseChunk[6:]
	var chunk map[string]interface{}
	if err := json.Unmarshal([]byte(jsonStr), &chunk); err != nil {
		return false
	}

	_, hasError := chunk["error"]
	return hasError
}
