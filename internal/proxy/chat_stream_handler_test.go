package proxy

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/eternisai/enchanted-proxy/internal/auth"
	"github.com/eternisai/enchanted-proxy/internal/logger"
	"github.com/eternisai/enchanted-proxy/internal/streaming"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

func setupChatStreamRouter(chatStreamManager *streaming.ChatStreamManager, streamManager *streaming.StreamManager, log *logger.Logger) *gin.Engine {
	gin.SetMode(gin.TestMode)
	router := gin.New()

	// Mock auth middleware
	router.Use(func(c *gin.Context) {
		c.Set(string(auth.UserIDKey), "test-user-123")
		c.Next()
	})

	api := router.Group("/api/v1")
	{
		chats := api.Group("/chats")
		{
			chats.GET("/:chatId/stream", ChatStreamHandler(log, chatStreamManager, nil))
		}
	}

	return router
}

func TestChatStreamHandler_Connection(t *testing.T) {
	log := logger.New(logger.Config{Level: slog.LevelError})
	streamManager := streaming.NewStreamManager(nil, log)
	chatStreamManager := streaming.NewChatStreamManager(streamManager, log)
	streamManager.SetChatStreamManager(chatStreamManager)
	defer chatStreamManager.Shutdown()
	defer streamManager.Shutdown()

	router := setupChatStreamRouter(chatStreamManager, streamManager, log)

	server := httptest.NewServer(router)
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/api/v1/chats/chat-123/stream"

	dialer := websocket.Dialer{
		HandshakeTimeout: 5 * time.Second,
	}

	conn, resp, err := dialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("failed to connect: %v", err)
	}
	defer conn.Close()

	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Errorf("expected status 101, got %d", resp.StatusCode)
	}

	// Connection successful - WebSocket upgrade worked
	// Note: Heartbeat testing requires waiting 30+ seconds which is too slow for unit tests
}

func TestChatStreamHandler_StreamNotification(t *testing.T) {
	log := logger.New(logger.Config{Level: slog.LevelError})
	streamManager := streaming.NewStreamManager(nil, log)
	chatStreamManager := streaming.NewChatStreamManager(streamManager, log)
	streamManager.SetChatStreamManager(chatStreamManager)
	defer chatStreamManager.Shutdown()
	defer streamManager.Shutdown()

	router := setupChatStreamRouter(chatStreamManager, streamManager, log)

	server := httptest.NewServer(router)
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/api/v1/chats/chat-123/stream"

	dialer := websocket.Dialer{
		HandshakeTimeout: 5 * time.Second,
	}

	conn, _, err := dialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("failed to connect: %v", err)
	}
	defer conn.Close()

	// Wait for WebSocket connection to be fully established
	time.Sleep(500 * time.Millisecond)

	// Create a slow stream so we have time to receive messages
	lines := []string{
		"data: {\"choices\":[{\"delta\":{\"content\":\"Hello\"}}]}",
		"data: {\"choices\":[{\"delta\":{\"content\":\" World\"}}]}",
		"data: [DONE]",
	}
	body := newSlowMockSSEStream(lines, 50*time.Millisecond)
	streamManager.GetOrCreateSession("chat-123", "msg-456", body)

	conn.SetReadDeadline(time.Now().Add(5 * time.Second))

	receivedStarted := false
	receivedChunks := 0
	receivedCompleted := false

	for i := 0; i < 20; i++ {
		_, data, err := conn.ReadMessage()
		if err != nil {
			break
		}

		var msg streaming.WebSocketMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			continue
		}

		switch msg.Type {
		case "stream_started":
			receivedStarted = true
			if msg.MessageID != "msg-456" {
				t.Errorf("expected message_id 'msg-456', got %s", msg.MessageID)
			}
		case "chunk":
			receivedChunks++
			if msg.Line == "" {
				t.Error("chunk should have line content")
			}
		case "stream_completed":
			receivedCompleted = true
			goto done
		case "heartbeat":
			continue
		}
	}

done:
	if !receivedStarted {
		t.Error("did not receive stream_started event")
	}
	if receivedChunks == 0 {
		t.Error("did not receive any chunks")
	}
	if !receivedCompleted {
		t.Error("did not receive stream_completed event")
	}
}

func TestChatStreamHandler_ReplayActive(t *testing.T) {
	log := logger.New(logger.Config{Level: slog.LevelError})
	streamManager := streaming.NewStreamManager(nil, log)
	chatStreamManager := streaming.NewChatStreamManager(streamManager, log)
	streamManager.SetChatStreamManager(chatStreamManager)
	defer chatStreamManager.Shutdown()
	defer streamManager.Shutdown()

	// Set up server first so we can connect immediately
	router := setupChatStreamRouter(chatStreamManager, streamManager, log)
	server := httptest.NewServer(router)
	defer server.Close()

	// Create a slow stream (10 chunks at 200ms = 2 seconds total)
	lines := make([]string, 10)
	for i := range lines {
		lines[i] = "data: {\"choices\":[{\"delta\":{\"content\":\"test\"}}]}"
	}
	lines = append(lines, "data: [DONE]")
	body := newSlowMockSSEStream(lines, 200*time.Millisecond)
	streamManager.GetOrCreateSession("chat-123", "msg-789", body)

	// Wait a bit for stream to start processing
	time.Sleep(300 * time.Millisecond)

	// Now connect with replay=true while stream is still active
	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/api/v1/chats/chat-123/stream?replay=true"

	dialer := websocket.Dialer{
		HandshakeTimeout: 5 * time.Second,
	}

	conn, _, err := dialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("failed to connect: %v", err)
	}
	defer conn.Close()

	// Should receive replayed chunks
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))

	receivedStarted := false
	receivedChunks := 0

	for i := 0; i < 20; i++ {
		_, data, err := conn.ReadMessage()
		if err != nil {
			break
		}

		var msg streaming.WebSocketMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			continue
		}

		switch msg.Type {
		case "stream_started":
			receivedStarted = true
		case "chunk":
			receivedChunks++
		case "heartbeat":
			continue
		}

		if receivedStarted && receivedChunks > 0 {
			break
		}
	}

	if !receivedStarted {
		t.Error("did not receive stream_started event during replay")
	}
	if receivedChunks == 0 {
		t.Error("did not receive replayed chunks")
	}
}

func TestChatStreamHandler_MultipleSubscribers(t *testing.T) {
	log := logger.New(logger.Config{Level: slog.LevelError})
	streamManager := streaming.NewStreamManager(nil, log)
	chatStreamManager := streaming.NewChatStreamManager(streamManager, log)
	streamManager.SetChatStreamManager(chatStreamManager)
	defer chatStreamManager.Shutdown()
	defer streamManager.Shutdown()

	router := setupChatStreamRouter(chatStreamManager, streamManager, log)
	server := httptest.NewServer(router)
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/api/v1/chats/chat-123/stream"

	dialer := websocket.Dialer{
		HandshakeTimeout: 5 * time.Second,
	}

	conn1, _, err := dialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("failed to connect subscriber 1: %v", err)
	}
	defer conn1.Close()

	conn2, _, err := dialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("failed to connect subscriber 2: %v", err)
	}
	defer conn2.Close()

	// Create a stream
	go func() {
		time.Sleep(200 * time.Millisecond)
		lines := []string{
			"data: {\"choices\":[{\"delta\":{\"content\":\"Test\"}}]}",
			"data: [DONE]",
		}
		body := newMockSSEStream(lines)
		streamManager.GetOrCreateSession("chat-123", "msg-multi", body)
	}()

	conn1.SetReadDeadline(time.Now().Add(5 * time.Second))
	conn2.SetReadDeadline(time.Now().Add(5 * time.Second))

	received1 := false
	received2 := false

	done := make(chan bool, 2)

	// Subscriber 1
	go func() {
		for i := 0; i < 10; i++ {
			_, data, err := conn1.ReadMessage()
			if err != nil {
				break
			}
			var msg streaming.WebSocketMessage
			if err := json.Unmarshal(data, &msg); err != nil {
				continue
			}
			if msg.Type == "stream_started" {
				received1 = true
				break
			}
		}
		done <- true
	}()

	// Subscriber 2
	go func() {
		for i := 0; i < 10; i++ {
			_, data, err := conn2.ReadMessage()
			if err != nil {
				break
			}
			var msg streaming.WebSocketMessage
			if err := json.Unmarshal(data, &msg); err != nil {
				continue
			}
			if msg.Type == "stream_started" {
				received2 = true
				break
			}
		}
		done <- true
	}()

	timeout := time.After(10 * time.Second)
	for i := 0; i < 2; i++ {
		select {
		case <-done:
		case <-timeout:
			t.Fatal("timeout waiting for subscribers")
		}
	}

	if !received1 {
		t.Error("subscriber 1 did not receive stream_started")
	}
	if !received2 {
		t.Error("subscriber 2 did not receive stream_started")
	}
}

func TestChatStreamHandler_Unauthenticated(t *testing.T) {
	log := logger.New(logger.Config{Level: slog.LevelError})
	streamManager := streaming.NewStreamManager(nil, log)
	chatStreamManager := streaming.NewChatStreamManager(streamManager, log)
	streamManager.SetChatStreamManager(chatStreamManager)
	defer chatStreamManager.Shutdown()
	defer streamManager.Shutdown()

	// Create router without auth middleware
	gin.SetMode(gin.TestMode)
	router := gin.New()
	api := router.Group("/api/v1")
	{
		chats := api.Group("/chats")
		{
			chats.GET("/:chatId/stream", ChatStreamHandler(log, chatStreamManager, nil))
		}
	}

	server := httptest.NewServer(router)
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/api/v1/chats/chat-123/stream"

	dialer := websocket.Dialer{
		HandshakeTimeout: 5 * time.Second,
	}

	_, resp, err := dialer.Dial(wsURL, nil)
	if err == nil {
		t.Error("expected connection to fail without auth")
	}

	if resp != nil && resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("expected status 401, got %d", resp.StatusCode)
	}
}

func TestChatStreamHandler_InvalidChatID(t *testing.T) {
	log := logger.New(logger.Config{Level: slog.LevelError})
	streamManager := streaming.NewStreamManager(nil, log)
	chatStreamManager := streaming.NewChatStreamManager(streamManager, log)
	streamManager.SetChatStreamManager(chatStreamManager)
	defer chatStreamManager.Shutdown()
	defer streamManager.Shutdown()

	router := setupChatStreamRouter(chatStreamManager, streamManager, log)
	server := httptest.NewServer(router)
	defer server.Close()

	// Chat ID that's too long
	longChatID := strings.Repeat("x", 257)
	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/api/v1/chats/" + longChatID + "/stream"

	dialer := websocket.Dialer{
		HandshakeTimeout: 5 * time.Second,
	}

	_, resp, err := dialer.Dial(wsURL, nil)
	if err == nil {
		t.Error("expected connection to fail with invalid chat ID")
	}

	if resp != nil && resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected status 400, got %d", resp.StatusCode)
	}
}
