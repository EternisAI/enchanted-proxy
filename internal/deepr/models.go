package deepr

import "time"

// Message represents a WebSocket message for deep research
type Message struct {
	Type        string `json:"type"`
	Content     string `json:"content"`
	Data        string `json:"data,omitempty"`
	FinalReport string `json:"final_report,omitempty"`
	Error       string `json:"error,omitempty"`
}

// Request represents a request to the deep research service
type Request struct {
	Query string `json:"query"`
	Type  string `json:"type"`
}

// Response represents a response from the deep research service
type Response struct {
	Type    string `json:"type"`
	Content string `json:"content"`
	Status  string `json:"status,omitempty"`
}

// PersistedMessage represents a message stored to disk
type PersistedMessage struct {
	ID          string    `json:"id"`
	UserID      string    `json:"user_id"`
	ChatID      string    `json:"chat_id"`
	Message     string    `json:"message"`
	Sent        bool      `json:"sent"`
	Timestamp   time.Time `json:"timestamp"`
	MessageType string    `json:"message_type"` // "status", "error", "final", etc.
}

// SessionState represents the state of a deep research session
type SessionState struct {
	UserID               string              `json:"user_id"`
	ChatID               string              `json:"chat_id"`
	Messages             []PersistedMessage  `json:"messages"`
	BackendConnected     bool                `json:"backend_connected"`
	LastActivity         time.Time           `json:"last_activity"`
	FinalReportReceived  bool                `json:"final_report_received"`
	ErrorOccurred        bool                `json:"error_occurred"`
}
