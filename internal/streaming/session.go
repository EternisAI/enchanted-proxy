package streaming

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/eternisai/enchanted-proxy/internal/logger"
)

const (
	// maxChunks is the maximum number of chunks to buffer per session
	// Protects against memory exhaustion from very long responses
	// ~10MB worst case (10,000 chunks × ~1KB each)
	maxChunks = 10000

	// maxChunkSize is the maximum size of a single chunk in bytes
	// Prevents individual chunks from consuming excessive memory
	maxChunkSize = 1024 * 1024 // 1MB

	// subscriberSendTimeout is how long to wait when sending to a slow subscriber
	// After this timeout, the chunk is dropped for that subscriber
	subscriberSendTimeout = 100 * time.Millisecond

	// upstreamReadTimeout is the maximum time to wait for upstream response
	// Prevents hanging forever if AI provider becomes unresponsive
	upstreamReadTimeout = 10 * time.Minute
)

// StreamSession manages a single AI response stream, broadcasting it to multiple clients.
//
// Key responsibilities:
//   - Read from upstream AI provider (OpenAI, Anthropic, etc.)
//   - Buffer all chunks for late-join replay
//   - Broadcast chunks to all subscribed clients (non-blocking)
//   - Extract and store complete message when done
//   - Handle user-initiated stop requests
//   - Track response_id for Responses API (GPT-5 Pro)
//
// Lifecycle:
//  1. Created by StreamManager when first client requests
//  2. Starts background goroutine to read upstream
//  3. Accepts subscriber subscriptions (concurrent)
//  4. Broadcasts chunks to all subscribers (non-blocking)
//  5. On completion: extract content, save to Firestore, mark completed
//  6. Kept alive for TTL (30min) for late joiners
//  7. Cleaned up by StreamManager
//
// Thread-safety:
//   - All methods are thread-safe (protected by mutexes)
//   - Multiple goroutines can subscribe/unsubscribe concurrently
//   - Broadcast loop doesn't block on slow subscribers
type StreamSession struct {
	// Identifiers
	chatID    string
	messageID string

	// Timing
	startTime       time.Time
	completedAt     time.Time
	stopRequestedAt time.Time

	// Upstream reading (background goroutine)
	upstreamBody io.ReadCloser
	completed    bool
	err          error
	completedMu  sync.RWMutex

	// Stop control
	stopCtx    context.Context    // Context for stopping upstream read
	stopCancel context.CancelFunc // Cancel function to stop reading
	stopped    bool               // Whether stream was stopped (user/system)
	stoppedBy  string             // User ID who stopped, or reason (e.g., "system_timeout")
	stopReason StopReason         // Why the stream was stopped
	stopMu     sync.RWMutex       // Protects stopped, stoppedBy, stopReason

	// Responses API support (for GPT-5 Pro and stateful models)
	responseID   string       // OpenAI Responses API response_id (e.g., "resp_abc123")
	responseIDMu sync.RWMutex // Protects responseID

	// Chunk storage (buffered for late-join replay)
	chunks   []StreamChunk
	chunksMu sync.RWMutex

	// Subscriber management
	subscribers   map[string]*StreamSubscriber
	subscribersMu sync.RWMutex

	// Tool execution
	toolExecutor *ToolExecutor

	// Request continuation (for tool calls)
	originalRequest []byte // Original request body for continuation
	requestMu       sync.RWMutex

	// Logger
	logger *logger.Logger
}

// NewStreamSession creates a new stream session.
//
// Parameters:
//   - chatID: Chat session identifier
//   - messageID: AI response message identifier
//   - upstreamBody: Response body from AI provider (typically SSE stream)
//   - logger: Logger for this session
//
// Returns:
//   - *StreamSession: The new session (not yet started)
//
// After creation, caller must:
//  1. Call Start() to begin reading upstream
//  2. Subscribe initial client(s)
//  3. StreamManager handles cleanup
func NewStreamSession(chatID, messageID string, upstreamBody io.ReadCloser, logger *logger.Logger) *StreamSession {
	// Create stoppable context from the start (independent of client connections)
	// This allows user-initiated stop while ensuring upstream reading completes regardless of client disconnects
	stopCtx, stopCancel := context.WithTimeout(context.Background(), upstreamReadTimeout)

	return &StreamSession{
		chatID:       chatID,
		messageID:    messageID,
		startTime:    time.Now(),
		upstreamBody: upstreamBody,
		stopCtx:      stopCtx,
		stopCancel:   stopCancel,
		chunks:       make([]StreamChunk, 0, 100), // Pre-allocate for typical response
		subscribers:  make(map[string]*StreamSubscriber),
		logger:       logger,
	}
}

// Start begins reading from upstream in a background goroutine.
// This must be called after creating the session.
//
// The goroutine will:
//  1. Read all SSE lines from upstream
//  2. Buffer chunks for replay
//  3. Broadcast to all subscribers
//  4. Handle completion or errors
//  5. Never block on slow subscribers
//
// Reading continues even if all subscribers disconnect (ensures complete message storage).
func (s *StreamSession) Start() {
	go s.readUpstream()
}

// SetToolExecutor sets the tool executor for this session.
func (s *StreamSession) SetToolExecutor(executor *ToolExecutor) {
	s.toolExecutor = executor
}

// SetOriginalRequest stores the original request body for tool call continuation.
func (s *StreamSession) SetOriginalRequest(requestBody []byte) {
	s.requestMu.Lock()
	defer s.requestMu.Unlock()
	s.originalRequest = requestBody
}

// readUpstream reads from the upstream AI provider and broadcasts to subscribers.
// This runs in a background goroutine and handles all upstream reading logic.
//
// Key behaviors:
//   - Uses background context (independent of client connections)
//   - Continues reading even if all clients disconnect
//   - Respects stop requests via stopCtx
//   - Handles panics gracefully
//   - Always marks session as completed when done
func (s *StreamSession) readUpstream() {
	// Panic recovery ensures one bad stream doesn't crash the server
	defer func() {
		if r := recover(); r != nil {
			s.logger.Error("panic in readUpstream",
				slog.Any("panic", r),
				slog.String("chat_id", s.chatID),
				slog.String("message_id", s.messageID))
			s.markCompleted(fmt.Errorf("panic: %v", r))
		}
	}()

	// Ensure cleanup
	defer func() {
		s.stopCancel() // Cancel context
		if s.upstreamBody != nil {
			s.upstreamBody.Close()
		}
	}()

	s.logger.Info("starting upstream read",
		slog.String("chat_id", s.chatID),
		slog.String("message_id", s.messageID))

	// Create scanner for SSE lines
	scanner := bufio.NewScanner(s.upstreamBody)
	scanner.Buffer(make([]byte, 64*1024), maxChunkSize) // 64KB initial, 1MB max

	chunkIndex := 0

	// Tool call detection (if tool executor is set)
	var toolDetector *ToolCallDetector
	if s.toolExecutor != nil {
		toolDetector = NewToolCallDetector()
	}

	for scanner.Scan() {
		// Check if stop was requested
		select {
		case <-s.stopCtx.Done():
			// Stopped by user or timeout
			reason := "unknown"
			if errors.Is(s.stopCtx.Err(), context.Canceled) {
				reason = "user stop or system cancel"
			} else if errors.Is(s.stopCtx.Err(), context.DeadlineExceeded) {
				reason = "timeout"
			}

			s.logger.Info("upstream read stopped",
				slog.String("reason", reason),
				slog.String("chat_id", s.chatID),
				slog.String("message_id", s.messageID),
				slog.Int("chunks_read", chunkIndex))

			s.markCompleted(s.stopCtx.Err())
			return
		default:
			// Continue processing
		}

		line := scanner.Text()

		// Skip empty lines (common in SSE streams)
		if strings.TrimSpace(line) == "" {
			continue
		}

		// Detect tool calls if executor is available
		if toolDetector != nil {
			toolDetector.ProcessChunk(line)
		}

		// Check if this is the final chunk
		isFinal := strings.Contains(line, "[DONE]")
		isError := strings.Contains(line, `"error"`)

		// Create chunk
		chunk := StreamChunk{
			Index:     chunkIndex,
			Line:      line,
			Timestamp: time.Now(),
			IsFinal:   isFinal,
			IsError:   isError,
		}

		// Store chunk (with safety limits)
		s.storeChunk(chunk)

		// Broadcast to all subscribers (non-blocking)
		s.broadcast(chunk)

		chunkIndex++

		// Check if tool calls are complete
		if toolDetector != nil && toolDetector.IsComplete() {
			s.logger.Info("tool calls detected, executing tools",
				slog.String("chat_id", s.chatID),
				slog.String("message_id", s.messageID))

			// Get tool calls
			toolCalls := toolDetector.GetToolCalls()

			// Execute tools (this sends WebSocket notifications)
			toolResults, err := s.toolExecutor.ExecuteToolCalls(s.stopCtx, s.chatID, s.messageID, toolCalls)
			if err != nil {
				s.logger.Error("tool execution failed",
					slog.String("error", err.Error()),
					slog.String("chat_id", s.chatID),
					slog.String("message_id", s.messageID))
				// Continue despite error - stream may still complete
				toolDetector = NewToolCallDetector()
				continue
			}

			// Create continuation request with tool results
			s.requestMu.RLock()
			originalRequest := s.originalRequest
			s.requestMu.RUnlock()

			if originalRequest != nil && len(toolResults) > 0 {
				s.logger.Info("creating continuation request with tool results",
					slog.String("chat_id", s.chatID),
					slog.String("message_id", s.messageID),
					slog.Int("tool_result_count", len(toolResults)))

				// Parse original request to get messages
				var originalReq map[string]interface{}
				if err := json.Unmarshal(originalRequest, &originalReq); err != nil {
					s.logger.Error("failed to parse original request",
						slog.String("error", err.Error()))
					toolDetector = NewToolCallDetector()
					continue
				}

				// Extract messages array
				originalMessages, ok := originalReq["messages"].([]interface{})
				if !ok {
					s.logger.Error("original request has no messages array")
					toolDetector = NewToolCallDetector()
					continue
				}

				// Convert to []map[string]interface{}
				messages := make([]map[string]interface{}, len(originalMessages))
				for i, msg := range originalMessages {
					if msgMap, ok := msg.(map[string]interface{}); ok {
						messages[i] = msgMap
					}
				}

				// Build assistant message from tool calls
				assistantMessage := map[string]interface{}{
					"role":       "assistant",
					"content":    nil, // Tool calls typically have no content
					"tool_calls": toolCalls,
				}

				// Create continuation request
				continuationBody, err := s.toolExecutor.CreateContinuationRequest(
					s.stopCtx,
					messages,
					assistantMessage,
					toolResults,
				)
				if err != nil {
					s.logger.Error("failed to create continuation request",
						slog.String("error", err.Error()))
					toolDetector = NewToolCallDetector()
					continue
				}

				// Close current upstream body
				if s.upstreamBody != nil {
					s.upstreamBody.Close()
				}

				// Replace with continuation body and continue reading
				s.upstreamBody = continuationBody
				scanner = bufio.NewScanner(s.upstreamBody)
				toolDetector = NewToolCallDetector() // Reset for next potential tool call

				s.logger.Info("continuation request created, resuming stream",
					slog.String("chat_id", s.chatID),
					slog.String("message_id", s.messageID))

				continue
			} else {
				s.logger.Warn("cannot create continuation: no original request or no tool results",
					slog.String("chat_id", s.chatID),
					slog.String("message_id", s.messageID),
					slog.Bool("has_original_request", originalRequest != nil),
					slog.Int("tool_result_count", len(toolResults)))
			}

			// Reset detector for potential subsequent tool calls
			toolDetector = NewToolCallDetector()
		}

		// If this is the final chunk, we're done
		if isFinal {
			break
		}
	}

	// Check for scanner errors
	if err := scanner.Err(); err != nil {
		s.logger.Error("scanner error while reading upstream",
			slog.String("error", err.Error()),
			slog.String("chat_id", s.chatID),
			slog.String("message_id", s.messageID))

		// Broadcast error chunk to subscribers
		errorChunk := StreamChunk{
			Index:     chunkIndex,
			Line:      fmt.Sprintf(`data: {"error": "upstream_error", "message": "%s"}`, err.Error()),
			Timestamp: time.Now(),
			IsFinal:   true,
			IsError:   true,
		}
		s.storeChunk(errorChunk)
		s.broadcast(errorChunk)

		s.markCompleted(err)
		return
	}

	s.logger.Info("upstream read completed",
		slog.String("chat_id", s.chatID),
		slog.String("message_id", s.messageID),
		slog.Int("total_chunks", chunkIndex))

	s.markCompleted(nil)
}

// storeChunk adds a chunk to the buffer with safety limits.
// Prevents memory exhaustion from very long responses.
func (s *StreamSession) storeChunk(chunk StreamChunk) {
	s.chunksMu.Lock()
	defer s.chunksMu.Unlock()

	// Safety: Truncate chunk if too large
	if len(chunk.Line) > maxChunkSize {
		s.logger.Warn("chunk too large, truncating",
			slog.Int("original_size", len(chunk.Line)),
			slog.Int("max_size", maxChunkSize),
			slog.String("chat_id", s.chatID))
		chunk.Line = chunk.Line[:maxChunkSize]
	}

	// Safety: If buffer is full, drop oldest chunks (keep first 100 and last chunks)
	if len(s.chunks) >= maxChunks {
		s.logger.Warn("chunk buffer full, dropping old chunks",
			slog.Int("buffer_size", len(s.chunks)),
			slog.String("chat_id", s.chatID))

		// Keep first 100 chunks (usually contain important metadata)
		// and most recent chunks (the actual content)
		s.chunks = append(s.chunks[:100], s.chunks[len(s.chunks)-9900:]...)
	}

	s.chunks = append(s.chunks, chunk)
}

// broadcast sends a chunk to all subscribers (non-blocking).
// Slow subscribers may miss chunks, but fast subscribers and upstream reading are not affected.
func (s *StreamSession) broadcast(chunk StreamChunk) {
	s.subscribersMu.RLock()
	defer s.subscribersMu.RUnlock()

	if len(s.subscribers) == 0 {
		// No subscribers, but we still buffer chunks for future late-joiners
		return
	}

	for id, sub := range s.subscribers {
		// Skip disconnected subscribers
		if sub.IsDisconnected() {
			continue
		}

		// Non-blocking send with timeout
		sent := sub.Send(chunk, subscriberSendTimeout)
		if !sent {
			s.logger.Warn("subscriber lagging, dropped chunk",
				slog.String("subscriber_id", id),
				slog.Int("chunk_index", chunk.Index),
				slog.String("chat_id", s.chatID))
		}
	}
}

// markCompleted marks the session as completed and performs cleanup.
func (s *StreamSession) markCompleted(err error) {
	s.completedMu.Lock()
	if s.completed {
		s.completedMu.Unlock()
		return // Already completed
	}
	s.completed = true
	s.completedAt = time.Now()
	s.err = err
	s.completedMu.Unlock()

	// Get chunk count under lock for logging
	s.chunksMu.RLock()
	chunkCount := len(s.chunks)
	s.chunksMu.RUnlock()

	s.logger.Info("stream session completed",
		slog.String("chat_id", s.chatID),
		slog.String("message_id", s.messageID),
		slog.Int("total_chunks", chunkCount),
		slog.Duration("duration", time.Since(s.startTime)),
		slog.Bool("has_error", err != nil))

	// Close all subscriber channels
	s.closeAllSubscribers()
}

// closeAllSubscribers closes all subscriber channels.
// Called when stream completes or is stopped.
func (s *StreamSession) closeAllSubscribers() {
	s.subscribersMu.Lock()
	defer s.subscribersMu.Unlock()

	for id, sub := range s.subscribers {
		sub.Cancel()
		sub.Close()
		s.logger.Debug("closed subscriber channel",
			slog.String("subscriber_id", id),
			slog.String("chat_id", s.chatID))
	}
}

// Subscribe adds a new subscriber to this stream.
//
// Parameters:
//   - ctx: Subscriber's context (typically HTTP request context)
//   - subscriberID: Unique identifier for this subscriber
//   - opts: Subscription options (replay, buffer size, etc.)
//
// Returns:
//   - *StreamSubscriber: The new subscriber (already added to session)
//   - error: If subscription failed
//
// Behavior:
//   - If opts.ReplayFromStart=true: Replays all buffered chunks before live chunks
//   - If stream is completed: Replays all chunks immediately and closes
//   - If stream is in progress: Receives live chunks only (unless replay=true)
//
// Thread-safe: Multiple goroutines can subscribe concurrently.
func (s *StreamSession) Subscribe(ctx context.Context, subscriberID string, opts SubscriberOptions) (*StreamSubscriber, error) {
	// Create subscriber
	sub := NewStreamSubscriber(ctx, subscriberID, opts)

	// Add to subscribers map
	s.subscribersMu.Lock()
	s.subscribers[subscriberID] = sub
	s.subscribersMu.Unlock()

	s.logger.Info("new subscriber joined",
		slog.String("subscriber_id", subscriberID),
		slog.String("chat_id", s.chatID),
		slog.String("message_id", s.messageID),
		slog.Bool("replay_from_start", opts.ReplayFromStart))

	// If replay requested or stream completed, send buffered chunks
	if opts.ReplayFromStart || s.IsCompleted() {
		go s.replayChunks(sub)
	}

	return sub, nil
}

// replayChunks sends all buffered chunks to a subscriber.
// Used for late-joiners or when stream has completed.
//
// Sends are blocking to ensure the subscriber receives all chunks in order.
func (s *StreamSession) replayChunks(sub *StreamSubscriber) {
	s.chunksMu.RLock()
	chunks := make([]StreamChunk, len(s.chunks))
	copy(chunks, s.chunks)
	s.chunksMu.RUnlock()

	s.logger.Debug("replaying chunks to subscriber",
		slog.String("subscriber_id", sub.ID),
		slog.Int("chunk_count", len(chunks)),
		slog.String("chat_id", s.chatID))

	for _, chunk := range chunks {
		if !sub.SendBlocking(chunk) {
			// Subscriber disconnected
			s.logger.Debug("subscriber disconnected during replay",
				slog.String("subscriber_id", sub.ID),
				slog.String("chat_id", s.chatID))
			return
		}
	}

	// If stream is completed, close the subscriber
	if s.IsCompleted() {
		sub.Cancel()
		sub.Close()
	}
}

// Unsubscribe removes a subscriber from this stream.
// Safe to call multiple times.
func (s *StreamSession) Unsubscribe(subscriberID string) {
	s.subscribersMu.Lock()
	defer s.subscribersMu.Unlock()

	if sub, exists := s.subscribers[subscriberID]; exists {
		sub.Cancel()
		// Don't close the channel here - let the goroutine reading from it handle that
		delete(s.subscribers, subscriberID)

		s.logger.Debug("subscriber unsubscribed",
			slog.String("subscriber_id", subscriberID),
			slog.String("chat_id", s.chatID))
	}
}

// Stop cancels the upstream read and broadcasts stop event to all clients.
//
// Parameters:
//   - stoppedBy: User ID who requested stop, or "system_timeout"/"system_shutdown"
//   - reason: Why the stream was stopped
//
// Returns:
//   - error: If stop failed (e.g., already completed)
//
// Behavior:
//   - Cancels upstream context (stops reading from AI provider)
//   - Broadcasts stop event to all subscribers
//   - Marks session as completed
//   - Stores partial response (handled by caller)
//
// Thread-safe: Multiple goroutines can call Stop concurrently (only first wins).
func (s *StreamSession) Stop(stoppedBy string, reason StopReason) error {
	s.stopMu.Lock()
	defer s.stopMu.Unlock()

	// Check if already stopped
	if s.stopped {
		return errors.New("stream already stopped")
	}

	// Check if already completed naturally
	if s.IsCompleted() {
		return errors.New("stream already completed")
	}

	s.stopped = true
	s.stoppedBy = stoppedBy
	s.stopReason = reason
	s.stopRequestedAt = time.Now()

	// Get current chunk count under lock for logging
	s.chunksMu.RLock()
	chunkCount := len(s.chunks)
	s.chunksMu.RUnlock()

	s.logger.Info("stopping stream",
		slog.String("stopped_by", stoppedBy),
		slog.String("reason", string(reason)),
		slog.String("chat_id", s.chatID),
		slog.String("message_id", s.messageID),
		slog.Int("chunks_generated", chunkCount))

	// Cancel upstream context - this will stop the readUpstream goroutine
	s.stopCancel()

	// Broadcast stop event to all subscribers
	// Note: Index will be set correctly by storeChunk
	stopEvent := StreamChunk{
		Index:     chunkCount,
		Line:      fmt.Sprintf(`event: stream_stopped\ndata: {"message_id":"%s","stopped_by":"%s","reason":"%s"}`, s.messageID, stoppedBy, reason),
		Timestamp: time.Now(),
		IsFinal:   true,
		IsError:   false,
	}
	s.storeChunk(stopEvent)
	s.broadcast(stopEvent)

	// Give a brief moment for the stop event to be delivered before readUpstream exits
	// readUpstream will detect stopCtx cancellation and call markCompleted, which closes channels
	time.Sleep(10 * time.Millisecond)

	return nil
}

// IsCompleted returns whether the upstream read has finished.
func (s *StreamSession) IsCompleted() bool {
	s.completedMu.RLock()
	defer s.completedMu.RUnlock()
	return s.completed
}

// IsStopped returns whether the stream was stopped by user/system.
func (s *StreamSession) IsStopped() bool {
	s.stopMu.RLock()
	defer s.stopMu.RUnlock()
	return s.stopped
}

// GetStopInfo returns information about why the stream was stopped.
// Returns empty strings if not stopped.
func (s *StreamSession) GetStopInfo() (stoppedBy string, reason StopReason) {
	s.stopMu.RLock()
	defer s.stopMu.RUnlock()
	return s.stoppedBy, s.stopReason
}

// GetStoredChunks returns a copy of all buffered chunks.
// Safe to call concurrently.
func (s *StreamSession) GetStoredChunks() []StreamChunk {
	s.chunksMu.RLock()
	defer s.chunksMu.RUnlock()

	chunks := make([]StreamChunk, len(s.chunks))
	copy(chunks, s.chunks)
	return chunks
}

// GetContent extracts the full message content from all buffered chunks.
// This is used when saving the complete message to Firestore.
//
// Returns:
//   - string: The complete message content (concatenated from all chunks)
//
// Note: This extracts content from OpenAI/Anthropic SSE format.
// Different providers may need different extraction logic.
func (s *StreamSession) GetContent() string {
	s.chunksMu.RLock()
	defer s.chunksMu.RUnlock()

	var content strings.Builder

	for _, chunk := range s.chunks {
		// Skip error chunks and events
		if chunk.IsError || !strings.HasPrefix(chunk.Line, "data: ") {
			continue
		}

		// Extract content delta from SSE line
		data := strings.TrimPrefix(chunk.Line, "data: ")
		if data == "[DONE]" {
			continue
		}

		// Parse JSON
		var parsed map[string]interface{}
		if err := json.Unmarshal([]byte(data), &parsed); err != nil {
			continue
		}

		// Extract content from choices[0].delta.content
		choices, ok := parsed["choices"].([]interface{})
		if !ok || len(choices) == 0 {
			continue
		}

		firstChoice, ok := choices[0].(map[string]interface{})
		if !ok {
			continue
		}

		delta, ok := firstChoice["delta"].(map[string]interface{})
		if !ok {
			continue
		}

		if contentStr, ok := delta["content"].(string); ok {
			content.WriteString(contentStr)
		}
	}

	return content.String()
}

// GetInfo returns metadata about this stream session.
// Used for observability and debugging.
func (s *StreamSession) GetInfo() StreamInfo {
	s.completedMu.RLock()
	completed := s.completed
	s.completedMu.RUnlock()

	s.stopMu.RLock()
	stopped := s.stopped
	stoppedBy := s.stoppedBy
	s.stopMu.RUnlock()

	s.subscribersMu.RLock()
	subscriberCount := len(s.subscribers)
	s.subscribersMu.RUnlock()

	s.chunksMu.RLock()
	chunksReceived := len(s.chunks)
	s.chunksMu.RUnlock()

	return StreamInfo{
		SessionKey:      s.chatID + ":" + s.messageID,
		ChatID:          s.chatID,
		MessageID:       s.messageID,
		StartTime:       s.startTime,
		SubscriberCount: subscriberCount,
		ChunksReceived:  chunksReceived,
		Completed:       completed,
		Stopped:         stopped,
		StoppedBy:       stoppedBy,
	}
}

// GetSubscriberCount returns the current number of subscribers.
func (s *StreamSession) GetSubscriberCount() int {
	s.subscribersMu.RLock()
	defer s.subscribersMu.RUnlock()
	return len(s.subscribers)
}

// GetError returns any error that occurred during upstream reading.
func (s *StreamSession) GetError() error {
	s.completedMu.RLock()
	defer s.completedMu.RUnlock()
	return s.err
}

// SetResponseID stores the OpenAI Responses API response_id for this session.
// This is called when we extract the response_id from the first chunk.
//
// Parameters:
//   - responseID: The response_id from OpenAI (e.g., "resp_abc123")
//
// Thread-safe: Can be called concurrently.
func (s *StreamSession) SetResponseID(responseID string) {
	s.responseIDMu.Lock()
	defer s.responseIDMu.Unlock()
	s.responseID = responseID
}

// GetResponseID returns the OpenAI Responses API response_id for this session.
//
// Returns:
//   - string: The response_id (e.g., "resp_abc123"), or empty string if not set
//
// Thread-safe: Can be called concurrently.
func (s *StreamSession) GetResponseID() string {
	s.responseIDMu.RLock()
	defer s.responseIDMu.RUnlock()
	return s.responseID
}
