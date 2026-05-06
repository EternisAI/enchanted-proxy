package background

import (
	"encoding/json"
	"time"
)

// ResponseStatus represents the status of an OpenAI background response.
type ResponseStatus struct {
	ID          string     `json:"id"`                     // Response ID (e.g., "resp_abc123")
	Status      string     `json:"status"`                 // "queued" | "in_progress" | "completed" | "failed"
	Model       string     `json:"model,omitempty"`        // Model ID
	CreatedAt   *UnixTime  `json:"created_at,omitempty"`   // When response was created
	CompletedAt *UnixTime  `json:"completed_at,omitempty"` // When response completed (if completed)
	Error       *ErrorInfo `json:"error,omitempty"`        // Error details (if failed)
}

// UnixTime handles Unix timestamp (integer) from OpenAI API.
// OpenAI returns timestamps as Unix seconds (integer), not RFC3339 strings.
type UnixTime struct {
	time.Time
}

// UnmarshalJSON handles both Unix timestamp integers and RFC3339 strings.
func (ut *UnixTime) UnmarshalJSON(b []byte) error {
	// Try to unmarshal as integer (Unix timestamp)
	var timestamp int64
	if err := json.Unmarshal(b, &timestamp); err == nil {
		ut.Time = time.Unix(timestamp, 0)
		return nil
	}

	// Fallback: try to unmarshal as RFC3339 string
	var str string
	if err := json.Unmarshal(b, &str); err != nil {
		return err
	}
	t, err := time.Parse(time.RFC3339, str)
	if err != nil {
		return err
	}
	ut.Time = t
	return nil
}

// ErrorInfo represents error information from OpenAI.
type ErrorInfo struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code,omitempty"`
}

// ResponseContent represents the full response content from OpenAI Responses API.
//
// NOTE: Responses API uses a different format than Chat Completions:
// - output: Array of items (reasoning + message items)
// - choices: Legacy format (for backwards compatibility, may not be present)
type ResponseContent struct {
	ID      string                   `json:"id"`
	Status  string                   `json:"status"`
	Model   string                   `json:"model"`
	Output  []map[string]interface{} `json:"output,omitempty"`  // Responses API format (primary)
	Choices []map[string]interface{} `json:"choices,omitempty"` // Legacy format (fallback)
	Usage   *UsageInfo               `json:"usage,omitempty"`
}

// UsageInfo represents token usage information.
//
// The OpenAI Responses API uses `input_tokens` / `output_tokens`; Chat
// Completions uses `prompt_tokens` / `completion_tokens`. Pointer fields
// distinguish "not present in payload" from "present and zero" — needed
// because a `> 0` discriminator would silently fall through to the other
// field name on a legitimate zero. Callers should use Prompt() / Completion().
type UsageInfo struct {
	PromptTokens     *int `json:"prompt_tokens,omitempty"`
	CompletionTokens *int `json:"completion_tokens,omitempty"`
	TotalTokens      int  `json:"total_tokens"`
	InputTokens      *int `json:"input_tokens,omitempty"`
	OutputTokens     *int `json:"output_tokens,omitempty"`
}

// Prompt returns the prompt/input token count from whichever field the
// upstream API populated.
func (u *UsageInfo) Prompt() int {
	if u.PromptTokens != nil {
		return *u.PromptTokens
	}
	if u.InputTokens != nil {
		return *u.InputTokens
	}
	return 0
}

// Completion returns the completion/output token count from whichever field
// the upstream API populated.
func (u *UsageInfo) Completion() int {
	if u.CompletionTokens != nil {
		return *u.CompletionTokens
	}
	if u.OutputTokens != nil {
		return *u.OutputTokens
	}
	return 0
}

// PollingJob represents a background polling job.
type PollingJob struct {
	ResponseID        string
	UserID            string
	ChatID            string
	MessageID         string
	Model             string
	EncryptionEnabled *bool
	StartedAt         time.Time
}

// MapStatusToGenerationState maps OpenAI status to Firestore generationState.
func MapStatusToGenerationState(openAIStatus string) string {
	switch openAIStatus {
	case "queued":
		return "thinking"
	case "in_progress":
		return "thinking"
	case "completed":
		return "completed"
	case "failed":
		return "failed"
	default:
		return "thinking"
	}
}
