package streaming

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/eternisai/enchanted-proxy/internal/logger"
	"github.com/gorilla/websocket"
)

// ChatStreamHub manages WebSocket connections for a single chat.
// It subscribes to all message streams in the chat and broadcasts events to all connected clients.
type ChatStreamHub struct {
	chatID        string
	subscribers   map[string]*ChatSubscriber
	mu            sync.RWMutex
	streamManager *StreamManager
	logger        *logger.Logger

	// Cleanup
	ctx    context.Context
	cancel context.CancelFunc
	closed bool
	wg     sync.WaitGroup
}

// ChatSubscriber represents a WebSocket connection for a chat.
type ChatSubscriber struct {
	id     string
	userID string
	conn   *websocket.Conn
	sendCh chan []byte
	mu     sync.Mutex

	// Options
	replay bool

	// Context for cancellation
	ctx    context.Context
	cancel context.CancelFunc
}

// WebSocketMessage represents a message sent over the WebSocket.
// Type can be: stream_started, chunk, stream_completed, stream_stopped, error, heartbeat
type WebSocketMessage struct {
	Type      string                 `json:"type"`
	MessageID string                 `json:"message_id,omitempty"`
	Timestamp string                 `json:"timestamp"`
	Data      map[string]interface{} `json:"data,omitempty"`
	Line      string                 `json:"line,omitempty"` // Raw SSE line for "chunk" events
}

// NewChatStreamHub creates a new chat stream hub.
func NewChatStreamHub(chatID string, streamManager *StreamManager, logger *logger.Logger) *ChatStreamHub {
	ctx, cancel := context.WithCancel(context.Background())

	hub := &ChatStreamHub{
		chatID:        chatID,
		subscribers:   make(map[string]*ChatSubscriber),
		streamManager: streamManager,
		logger:        logger.WithComponent("chat-stream-hub"),
		ctx:           ctx,
		cancel:        cancel,
	}

	logger.Info("chat stream hub created", slog.String("chat_id", chatID))

	return hub
}

// Subscribe adds a new WebSocket connection to this chat.
func (h *ChatStreamHub) Subscribe(ctx context.Context, subscriberID, userID string, conn *websocket.Conn, replay bool) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.closed {
		return fmt.Errorf("hub is closed")
	}

	subCtx, subCancel := context.WithCancel(ctx)

	sub := &ChatSubscriber{
		id:     subscriberID,
		userID: userID,
		conn:   conn,
		sendCh: make(chan []byte, 100),
		replay: replay,
		ctx:    subCtx,
		cancel: subCancel,
	}

	h.subscribers[subscriberID] = sub

	h.logger.Info("subscriber added",
		slog.String("subscriber_id", subscriberID),
		slog.String("user_id", userID),
		slog.String("chat_id", h.chatID),
		slog.Int("total_subscribers", len(h.subscribers)))

	// Start send loop for this subscriber
	h.wg.Add(1)
	go func() {
		defer h.wg.Done()
		h.sendLoop(sub)
	}()

	// If replay requested, send active stream info
	if replay {
		h.wg.Add(1)
		go func() {
			defer h.wg.Done()
			h.replayActiveStreams(sub)
		}()
	}

	return nil
}

// replayActiveStreams replays any currently active stream to a new subscriber.
func (h *ChatStreamHub) replayActiveStreams(sub *ChatSubscriber) {
	streamInfo := h.streamManager.GetActiveStreamForChat(h.chatID)
	if streamInfo == nil {
		return
	}

	session := h.streamManager.GetSession(h.chatID, streamInfo.MessageID)
	if session == nil {
		return
	}

	msg := WebSocketMessage{
		Type:      "stream_started",
		MessageID: streamInfo.MessageID,
		Timestamp: streamInfo.StartTime.Format(time.RFC3339),
		Data: map[string]interface{}{
			"chat_id":    h.chatID,
			"message_id": streamInfo.MessageID,
		},
	}
	h.sendToSubscriber(sub, msg)

	subscriberID := fmt.Sprintf("%s-replay", sub.id)
	streamSub, err := session.Subscribe(sub.ctx, subscriberID, SubscriberOptions{
		ReplayFromStart: true,
		BufferSize:      100,
	})
	if err != nil {
		h.logger.Error("failed to subscribe to session for replay",
			slog.String("error", err.Error()),
			slog.String("message_id", streamInfo.MessageID))
		return
	}

	for chunk := range streamSub.Ch {
		chunkMsg := WebSocketMessage{
			Type:      "chunk",
			MessageID: streamInfo.MessageID,
			Timestamp: chunk.Timestamp.Format(time.RFC3339),
			Line:      chunk.Line,
		}

		h.sendToSubscriber(sub, chunkMsg)

		if chunk.IsFinal {
			break
		}
	}

	session.Unsubscribe(subscriberID)
}

// Unsubscribe removes a WebSocket connection from this chat.
func (h *ChatStreamHub) Unsubscribe(subscriberID string) {
	h.mu.Lock()
	defer h.mu.Unlock()

	sub, exists := h.subscribers[subscriberID]
	if !exists {
		return
	}

	sub.cancel()
	// Note: Don't close sub.sendCh here - other goroutines might still be sending
	// The channel will be garbage collected when all references are gone
	delete(h.subscribers, subscriberID)

	h.logger.Info("subscriber removed",
		slog.String("subscriber_id", subscriberID),
		slog.String("chat_id", h.chatID),
		slog.Int("remaining_subscribers", len(h.subscribers)))
}

// GetSubscriberCount returns the number of active subscribers.
func (h *ChatStreamHub) GetSubscriberCount() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.subscribers)
}

// OnStreamStarted is called when a new stream starts in this chat.
func (h *ChatStreamHub) OnStreamStarted(messageID string, session *StreamSession) {
	h.logger.Info("stream started in chat",
		slog.String("chat_id", h.chatID),
		slog.String("message_id", messageID))

	// Broadcast stream_started event
	msg := WebSocketMessage{
		Type:      "stream_started",
		MessageID: messageID,
		Timestamp: time.Now().Format(time.RFC3339),
		Data: map[string]interface{}{
			"chat_id":    h.chatID,
			"message_id": messageID,
		},
	}

	h.broadcast(msg)

	// Subscribe to the session for all subscribers
	h.wg.Add(1)
	go func() {
		defer h.wg.Done()
		h.subscribeToSession(messageID, session)
	}()
}

// OnToolCallStarted broadcasts tool_call_started event.
func (h *ChatStreamHub) OnToolCallStarted(messageID, toolName, toolCallID string) {
	h.logger.Info("tool call started",
		slog.String("chat_id", h.chatID),
		slog.String("message_id", messageID),
		slog.String("tool_name", toolName),
		slog.String("tool_call_id", toolCallID))

	msg := WebSocketMessage{
		Type:      "tool_call_started",
		MessageID: messageID,
		Timestamp: time.Now().Format(time.RFC3339),
		Data: map[string]interface{}{
			"tool_name":    toolName,
			"tool_call_id": toolCallID,
		},
	}

	h.broadcast(msg)
}

// OnToolCallCompleted broadcasts tool_call_completed event.
func (h *ChatStreamHub) OnToolCallCompleted(messageID, toolName, toolCallID, resultSummary string) {
	h.logger.Info("tool call completed",
		slog.String("chat_id", h.chatID),
		slog.String("message_id", messageID),
		slog.String("tool_name", toolName),
		slog.String("tool_call_id", toolCallID))

	msg := WebSocketMessage{
		Type:      "tool_call_completed",
		MessageID: messageID,
		Timestamp: time.Now().Format(time.RFC3339),
		Data: map[string]interface{}{
			"tool_name":      toolName,
			"tool_call_id":   toolCallID,
			"result_summary": resultSummary,
		},
	}

	h.broadcast(msg)
}

// OnToolCallError broadcasts tool_call_error event.
func (h *ChatStreamHub) OnToolCallError(messageID, toolName, toolCallID, errorMsg string) {
	h.logger.Error("tool call error",
		slog.String("chat_id", h.chatID),
		slog.String("message_id", messageID),
		slog.String("tool_name", toolName),
		slog.String("tool_call_id", toolCallID),
		slog.String("error", errorMsg))

	msg := WebSocketMessage{
		Type:      "tool_call_error",
		MessageID: messageID,
		Timestamp: time.Now().Format(time.RFC3339),
		Data: map[string]interface{}{
			"tool_name":    toolName,
			"tool_call_id": toolCallID,
			"error":        errorMsg,
		},
	}

	h.broadcast(msg)
}

// subscribeToSession subscribes to a stream session and forwards chunks to all subscribers.
func (h *ChatStreamHub) subscribeToSession(messageID string, session *StreamSession) {

	subscriberID := fmt.Sprintf("hub-%s-%s", h.chatID, messageID)

	streamSub, err := session.Subscribe(h.ctx, subscriberID, SubscriberOptions{
		ReplayFromStart: false,
		BufferSize:      100,
	})
	if err != nil {
		h.logger.Error("failed to subscribe to session",
			slog.String("error", err.Error()),
			slog.String("message_id", messageID))
		return
	}

	defer session.Unsubscribe(subscriberID)

	for chunk := range streamSub.Ch {
		msg := WebSocketMessage{
			Type:      "chunk",
			MessageID: messageID,
			Timestamp: chunk.Timestamp.Format(time.RFC3339),
			Line:      chunk.Line,
		}

		h.broadcast(msg)

		if chunk.IsFinal {
			info := session.GetInfo()
			completedMsg := WebSocketMessage{
				Type:      "stream_completed",
				MessageID: messageID,
				Timestamp: time.Now().Format(time.RFC3339),
				Data: map[string]interface{}{
					"chat_id":        h.chatID,
					"message_id":     messageID,
					"chunks_count":   info.ChunksReceived,
					"content_length": len(session.GetContent()),
				},
			}
			h.broadcast(completedMsg)
			break
		}
	}
}

// broadcast sends a message to all subscribers.
func (h *ChatStreamHub) broadcast(msg WebSocketMessage) {
	h.mu.RLock()
	defer h.mu.RUnlock()

	if len(h.subscribers) == 0 {
		return
	}

	data, err := json.Marshal(msg)
	if err != nil {
		h.logger.Error("failed to marshal message",
			slog.String("error", err.Error()))
		return
	}

	for _, sub := range h.subscribers {
		select {
		case sub.sendCh <- data:
			// Sent successfully
		default:
			// Channel full, subscriber is slow
			h.logger.Warn("subscriber channel full, dropping message",
				slog.String("subscriber_id", sub.id),
				slog.String("chat_id", h.chatID))
		}
	}
}

// sendToSubscriber sends a message to a specific subscriber.
func (h *ChatStreamHub) sendToSubscriber(sub *ChatSubscriber, msg WebSocketMessage) {
	// Check if subscriber is still active
	select {
	case <-sub.ctx.Done():
		return
	default:
	}

	data, err := json.Marshal(msg)
	if err != nil {
		h.logger.Error("failed to marshal message",
			slog.String("error", err.Error()))
		return
	}

	select {
	case sub.sendCh <- data:
		// Sent successfully
	case <-sub.ctx.Done():
		// Subscriber disconnected
	}
}

// sendLoop handles sending messages to a subscriber's WebSocket connection.
func (h *ChatStreamHub) sendLoop(sub *ChatSubscriber) {
	defer func() {
		sub.conn.Close()
		h.Unsubscribe(sub.id)
	}()

	heartbeatTicker := time.NewTicker(30 * time.Second)
	defer heartbeatTicker.Stop()

	for {
		select {
		case data := <-sub.sendCh:
			sub.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))

			sub.mu.Lock()
			err := sub.conn.WriteMessage(websocket.TextMessage, data)
			sub.mu.Unlock()

			if err != nil {
				h.logger.Error("failed to write to websocket",
					slog.String("error", err.Error()),
					slog.String("subscriber_id", sub.id))
				return
			}

		case <-sub.ctx.Done():
			// Context cancelled, exit cleanly
			return

		case <-heartbeatTicker.C:
			msg := WebSocketMessage{
				Type:      "heartbeat",
				Timestamp: time.Now().Format(time.RFC3339),
			}

			data, err := json.Marshal(msg)
			if err != nil {
				continue
			}

			sub.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))

			sub.mu.Lock()
			err = sub.conn.WriteMessage(websocket.TextMessage, data)
			sub.mu.Unlock()

			if err != nil {
				h.logger.Error("failed to send heartbeat",
					slog.String("error", err.Error()),
					slog.String("subscriber_id", sub.id))
				return
			}

		case <-sub.ctx.Done():
			return

		case <-h.ctx.Done():
			return
		}
	}
}

// Close closes the hub and all subscribers.
func (h *ChatStreamHub) Close() {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return
	}

	h.closed = true
	h.cancel()

	// Cancel all subscriber contexts
	for _, sub := range h.subscribers {
		sub.cancel()
	}
	h.mu.Unlock()

	// Wait for all goroutines to finish
	h.wg.Wait()

	// Clean up subscriber map (channels will be garbage collected)
	h.mu.Lock()
	h.subscribers = make(map[string]*ChatSubscriber)
	h.mu.Unlock()

	h.logger.Info("chat stream hub closed", slog.String("chat_id", h.chatID))
}
