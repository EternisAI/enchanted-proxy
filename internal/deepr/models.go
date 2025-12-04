package deepr

import "time"

// Message represents a WebSocket message for deep research.
type Message struct {
	Type        string `json:"type"`
	Content     string `json:"content"`
	Message     string `json:"message,omitempty"`  // Used by Python backend for clarifications
	Reporter    string `json:"reporter,omitempty"` // Message reporter type from Python backend
	Data        string `json:"data,omitempty"`
	FinalReport string `json:"final_report,omitempty"`
	Error       string `json:"error,omitempty"`
	TokensUsed  int    `json:"tokens_used,omitempty"` // Token usage for this message (if reported by backend)
}

// Request represents a request to the deep research service.
type Request struct {
	Query string `json:"query"`
	Type  string `json:"type"`
}

// Response represents a response from the deep research service.
type Response struct {
	Type    string `json:"type"`
	Content string `json:"content"`
	Status  string `json:"status,omitempty"`
}

// PersistedMessage represents a message stored in the database.
type PersistedMessage struct {
	ID          string    `json:"id"`
	UserID      string    `json:"user_id"`
	ChatID      string    `json:"chat_id"`
	Message     string    `json:"message"`
	Sent        bool      `json:"sent"`
	Timestamp   time.Time `json:"timestamp"`
	MessageType string    `json:"message_type"` // "status", "error", "final", etc.
}
