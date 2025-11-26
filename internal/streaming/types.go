package streaming

import (
	"time"
)

// StreamChunk represents a single SSE line from the AI provider.
// We store raw SSE lines rather than parsed JSON for several reasons:
//   - Provider-agnostic: Works with OpenAI, Anthropic, OpenRouter without changes
//   - Forward-compatible: New fields from providers don't break our system
//   - Debugging: Can inspect exact bytes received from upstream
//   - Simplicity: No need to understand every provider's response format
//
// Parsing happens at two points:
//  1. In proxy handler for real-time client streaming
//  2. In session completion for message storage
type StreamChunk struct {
	// Index is the sequential position in the stream (0, 1, 2, ...)
	Index int `json:"index"`

	// Line is the raw SSE line (e.g., "data: {...}" or "event: tool_result")
	Line string `json:"line"`

	// Timestamp is when this chunk was received from upstream
	Timestamp time.Time `json:"timestamp"`

	// IsFinal indicates this is the last chunk in the stream (typically "data: [DONE]")
	IsFinal bool `json:"is_final"`

	// IsError indicates this chunk contains an error message
	IsError bool `json:"is_error"`
}

// StreamInfo provides metadata about an active stream session.
// Used for observability and debugging.
type StreamInfo struct {
	// SessionKey uniquely identifies the stream ("chatID:messageID")
	SessionKey string `json:"session_key"`

	// ChatID is the chat session identifier
	ChatID string `json:"chat_id"`

	// MessageID is the AI response message identifier
	MessageID string `json:"message_id"`

	// StartTime is when the stream session was created
	StartTime time.Time `json:"start_time"`

	// SubscriberCount is the number of clients currently watching this stream
	SubscriberCount int `json:"subscriber_count"`

	// ChunksReceived is the total number of chunks received so far
	ChunksReceived int `json:"chunks_received"`

	// Completed indicates whether the upstream read has finished
	Completed bool `json:"completed"`

	// Stopped indicates whether the stream was stopped by user/system
	Stopped bool `json:"stopped"`

	// StoppedBy is the user ID who stopped the stream, or "system_timeout"
	StoppedBy string `json:"stopped_by,omitempty"`
}

// StreamMetrics provides aggregated metrics across all streams.
// Used for monitoring and alerting.
type StreamMetrics struct {
	// ActiveStreams is the number of currently active stream sessions
	ActiveStreams int `json:"active_streams"`

	// TotalSubscribers is the total number of connected clients across all streams
	TotalSubscribers int `json:"total_subscribers"`

	// CompletedStreams is the number of completed sessions still in memory (within TTL)
	CompletedStreams int `json:"completed_streams"`

	// MemoryUsageBytes is an estimate of memory used by buffered chunks
	MemoryUsageBytes int64 `json:"memory_usage_bytes"`
}

// StopReason indicates why a stream was stopped
type StopReason string

const (
	// StopReasonUserCancelled indicates the user requested to stop generation
	StopReasonUserCancelled StopReason = "user_cancelled"

	// StopReasonTimeout indicates the stream exceeded the maximum duration
	StopReasonTimeout StopReason = "timeout"

	// StopReasonError indicates an upstream error forced the stream to stop
	StopReasonError StopReason = "error"

	// StopReasonSystemShutdown indicates the server is shutting down
	StopReasonSystemShutdown StopReason = "system_shutdown"
)

// SubscriberOptions configures how a subscriber receives stream data
type SubscriberOptions struct {
	// ReplayFromStart indicates whether to send all buffered chunks before live chunks
	// Used for late-joiners who want to see the full response from the beginning
	ReplayFromStart bool

	// BufferSize is the capacity of the subscriber's channel
	// Larger buffers handle burst traffic better but use more memory
	// Default: 100
	BufferSize int
}

// DefaultSubscriberOptions returns default options for subscribers
func DefaultSubscriberOptions() SubscriberOptions {
	return SubscriberOptions{
		ReplayFromStart: false,
		BufferSize:      100,
	}
}
