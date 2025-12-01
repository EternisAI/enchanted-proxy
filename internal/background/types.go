package background

import "time"

// ResponseStatus represents the status of an OpenAI background response.
type ResponseStatus struct {
	ID                string    `json:"id"`                  // Response ID (e.g., "resp_abc123")
	Status            string    `json:"status"`              // "queued" | "in_progress" | "completed" | "failed"
	Model             string    `json:"model,omitempty"`     // Model ID
	CreatedAt         time.Time `json:"created_at"`          // When response was created
	CompletedAt       time.Time `json:"completed_at,omitempty"` // When response completed (if completed)
	Error             *ErrorInfo `json:"error,omitempty"`    // Error details (if failed)
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
type UsageInfo struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
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
