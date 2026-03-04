package streaming

import (
	"encoding/json"
	"strings"
)

// NormalizeSSEErrorLine detects non-JSON upstream error lines in SSE streams and converts
// them into valid OpenAI-format SSE chunks that clients can parse.
//
// Some providers (e.g., NEAR AI) emit plain-text error lines mid-stream like:
//
//	data: error: Failed to perform completion: error decoding response body
//
// These are not valid JSON and cause client-side parse failures. This function converts
// them into a properly formatted SSE chunk with finish_reason "error" and the error
// message as the final content delta, so clients degrade gracefully.
//
// Returns the normalized line and true if a transformation was applied.
func NormalizeSSEErrorLine(line string) (string, bool) {
	// Only handle "data: " lines that are NOT JSON and contain an error
	if !strings.HasPrefix(line, "data: ") {
		return line, false
	}

	payload := strings.TrimPrefix(line, "data: ")

	// Skip JSON payloads (normal SSE chunks) and [DONE]
	if strings.HasPrefix(payload, "{") || payload == "[DONE]" {
		return line, false
	}

	// This is a non-JSON data line — likely a plain-text error from the provider.
	// Examples seen in the wild:
	//   data: error: Failed to perform completion: error decoding response body
	//   data: Internal Server Error
	errorMsg := payload

	// Build a valid OpenAI-format SSE chunk that signals the error to the client.
	// We use finish_reason "stop" (not "error") because "error" is not in the OpenAI
	// finish_reason enum and would cause additional parse failures in strict clients.
	// The error is communicated as a content delta so the user sees what happened.
	chunk := map[string]interface{}{
		"id":      "error",
		"object":  "chat.completion.chunk",
		"created": 0,
		"model":   "unknown",
		"choices": []map[string]interface{}{
			{
				"index": 0,
				"delta": map[string]interface{}{
					"content": "\n\n[Stream error: " + errorMsg + "]",
				},
				"finish_reason": "stop",
			},
		},
	}

	jsonBytes, err := json.Marshal(chunk)
	if err != nil {
		// Shouldn't happen, but if it does, drop the bad line
		return line, false
	}

	return "data: " + string(jsonBytes), true
}
