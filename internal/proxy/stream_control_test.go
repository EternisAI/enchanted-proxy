package proxy

import (
	"encoding/json"
	"io"
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
)

// mockReadCloser for testing
type mockReadCloser struct {
	reader io.Reader
	closed bool
}

func (m *mockReadCloser) Read(p []byte) (n int, err error) {
	return m.reader.Read(p)
}

func (m *mockReadCloser) Close() error {
	m.closed = true
	return nil
}

func newMockSSEStream(lines []string) io.ReadCloser {
	content := strings.Join(lines, "\n")
	return &mockReadCloser{reader: strings.NewReader(content)}
}

// slowMockReadCloser for testing with delays
type slowMockReadCloser struct {
	reader *strings.Reader
	delay  time.Duration
}

func (s *slowMockReadCloser) Read(p []byte) (n int, err error) {
	if s.delay > 0 {
		time.Sleep(s.delay)
	}
	return s.reader.Read(p)
}

func (s *slowMockReadCloser) Close() error {
	return nil
}

func newSlowMockSSEStream(lines []string, delayPerLine time.Duration) io.ReadCloser {
	content := strings.Join(lines, "\n")
	return &slowMockReadCloser{
		reader: strings.NewReader(content),
		delay:  delayPerLine,
	}
}

// setupTestRouter creates a test router with auth middleware
func setupTestRouter(streamManager *streaming.StreamManager, log *logger.Logger) *gin.Engine {
	gin.SetMode(gin.TestMode)
	router := gin.New()

	// Mock auth middleware that sets user ID
	router.Use(func(c *gin.Context) {
		c.Set(string(auth.UserIDKey), "test-user-123")
		c.Next()
	})

	// Register routes
	// Pass nil for firestoreClient in tests to skip authorization checks
	// This keeps tests focused on handler logic rather than firestore integration
	api := router.Group("/api/v1")
	{
		chats := api.Group("/chats")
		{
			messages := chats.Group("/:chatId/messages")
			{
				messages.POST("/:messageId/stop", StopStreamHandler(log, streamManager, nil))
			}
		}
	}

	return router
}

func TestStopStreamHandler_Success(t *testing.T) {
	log := logger.New(logger.Config{Level: slog.LevelError})
	streamManager := streaming.NewStreamManager(nil, log)

	// Create a slow stream (200ms delay per read = very slow)
	lines := make([]string, 20)
	for i := range lines {
		lines[i] = "data: {\"choices\":[{\"delta\":{\"content\":\"test\"}}]}"
	}
	lines = append(lines, "data: [DONE]")

	body := newSlowMockSSEStream(lines, 200*time.Millisecond)
	session, _ := streamManager.GetOrCreateSession("chat-123", "msg-456", body)

	// Give stream time to start but not complete
	time.Sleep(300 * time.Millisecond)

	// Setup router
	router := setupTestRouter(streamManager, log)

	// Make stop request
	req := httptest.NewRequest("POST", "/api/v1/chats/chat-123/messages/msg-456/stop", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	// Check response
	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", w.Code)
	}

	var response map[string]interface{}
	err := json.Unmarshal(w.Body.Bytes(), &response)
	if err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}

	if !response["stopped"].(bool) {
		t.Error("expected stopped to be true")
	}
	if response["message_id"].(string) != "msg-456" {
		t.Errorf("expected message_id 'msg-456', got %s", response["message_id"])
	}
	if _, ok := response["chunks_generated"]; !ok {
		t.Error("expected chunks_generated in response")
	}

	// Verify session is stopped
	if !session.IsStopped() {
		t.Error("session should be stopped")
	}
}

func TestStopStreamHandler_NotFound(t *testing.T) {
	log := logger.New(logger.Config{Level: slog.LevelError})
	streamManager := streaming.NewStreamManager(nil, log)
	router := setupTestRouter(streamManager, log)

	// Try to stop non-existent stream
	req := httptest.NewRequest("POST", "/api/v1/chats/chat-123/messages/msg-999/stop", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected status 404, got %d", w.Code)
	}

	var response map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &response)
	if response["error"] == nil {
		t.Error("expected error message in response")
	}
}

func TestStopStreamHandler_AlreadyCompleted(t *testing.T) {
	log := logger.New(logger.Config{Level: slog.LevelError})
	streamManager := streaming.NewStreamManager(nil, log)

	// Create a stream that completes quickly
	lines := []string{
		"data: {\"choices\":[{\"delta\":{\"content\":\"test\"}}]}",
		"data: [DONE]",
	}
	body := newMockSSEStream(lines)
	_, _ = streamManager.GetOrCreateSession("chat-123", "msg-456", body)

	// Wait for completion
	time.Sleep(100 * time.Millisecond)

	router := setupTestRouter(streamManager, log)

	// Try to stop completed stream
	req := httptest.NewRequest("POST", "/api/v1/chats/chat-123/messages/msg-456/stop", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusConflict {
		t.Errorf("expected status 409, got %d", w.Code)
	}

	var response map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &response)
	if response["error"] == nil {
		t.Error("expected error message in response")
	}
	if !response["completed"].(bool) {
		t.Error("expected completed to be true")
	}
}

// Test removed: GetStreamHandler replaced by WebSocket-based ChatStreamHandler
// Old endpoint: GET /api/v1/chats/:chatId/messages/:messageId/stream
// New endpoint: WS /api/v1/chats/:chatId/stream

// Test removed: GetStreamHandler replaced by WebSocket-based ChatStreamHandler

// Test removed: GetStreamHandler replaced by WebSocket-based ChatStreamHandler

// Test removed: GetStreamHandler replaced by WebSocket-based ChatStreamHandler

// Test removed: GetStreamHandler replaced by WebSocket-based ChatStreamHandler

// Test removed: StreamStatusHandler replaced by WebSocket-based ChatStreamHandler

// Test removed: StreamStatusHandler replaced by WebSocket-based ChatStreamHandler

// Test removed: StreamStatusHandler replaced by WebSocket-based ChatStreamHandler

// Test removed: StreamStatusHandler replaced by WebSocket-based ChatStreamHandler

// ============================================================================
// INPUT VALIDATION TESTS
// ============================================================================

func TestStopStreamHandler_IDTooLong(t *testing.T) {
	log := logger.New(logger.Config{Level: slog.LevelError})
	streamManager := streaming.NewStreamManager(nil, log)
	router := setupTestRouter(streamManager, log)

	// Create a chat ID that exceeds maxChatIDLength (256)
	longChatID := strings.Repeat("a", 257)

	req, _ := http.NewRequest("POST", "/api/v1/chats/"+longChatID+"/messages/msg-456/stop", nil)
	w := httptest.NewRecorder()

	router.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected status 400, got %d", w.Code)
	}

	var response map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &response)

	if !strings.Contains(response["error"].(string), "exceeds maximum length") {
		t.Error("expected error message about ID length")
	}
}

// Test removed: GetStreamHandler replaced by WebSocket-based ChatStreamHandler

// Test removed: StreamStatusHandler replaced by WebSocket-based ChatStreamHandler

// ============================================================================
// AUTHENTICATION TESTS
// ============================================================================

func TestStopStreamHandler_Unauthenticated(t *testing.T) {
	log := logger.New(logger.Config{Level: slog.LevelError})
	streamManager := streaming.NewStreamManager(nil, log)

	// Create router without auth middleware
	gin.SetMode(gin.TestMode)
	router := gin.New()
	api := router.Group("/api/v1")
	{
		chats := api.Group("/chats")
		{
			messages := chats.Group("/:chatId/messages")
			{
				messages.POST("/:messageId/stop", StopStreamHandler(log, streamManager, nil))
			}
		}
	}

	req, _ := http.NewRequest("POST", "/api/v1/chats/chat-123/messages/msg-456/stop", nil)
	w := httptest.NewRecorder()

	router.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected status 401, got %d", w.Code)
	}

	var response map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &response)

	if !strings.Contains(response["error"].(string), "Authentication required") {
		t.Error("expected authentication required error")
	}
}

// Test removed: GetStreamHandler replaced by WebSocket-based ChatStreamHandler

// Test removed: StreamStatusHandler replaced by WebSocket-based ChatStreamHandler

// ============================================================================
// EDGE CASE TESTS
// ============================================================================

func TestStopStreamHandler_ConcurrentStops(t *testing.T) {
	log := logger.New(logger.Config{Level: slog.LevelError})
	streamManager := streaming.NewStreamManager(nil, log)

	// Create a slow stream
	lines := make([]string, 20)
	for i := range lines {
		lines[i] = "data: {\"choices\":[{\"delta\":{\"content\":\"test\"}}]}"
	}
	lines = append(lines, "data: [DONE]")
	body := newSlowMockSSEStream(lines, 200*time.Millisecond)

	// Start session
	_, _ = streamManager.GetOrCreateSession("chat-concurrent", "msg-concurrent", body)
	time.Sleep(200 * time.Millisecond) // Ensure stream is running

	router := setupTestRouter(streamManager, log)

	// Send multiple concurrent stop requests
	numRequests := 5
	responses := make(chan int, numRequests)

	for i := 0; i < numRequests; i++ {
		go func() {
			req, _ := http.NewRequest("POST", "/api/v1/chats/chat-concurrent/messages/msg-concurrent/stop", nil)
			w := httptest.NewRecorder()
			router.ServeHTTP(w, req)
			responses <- w.Code
		}()
	}

	// Collect responses
	statusCodes := make(map[int]int)
	for i := 0; i < numRequests; i++ {
		code := <-responses
		statusCodes[code]++
	}

	// Should have at least one 200 (success) and possibly some 409 (already completed)
	if statusCodes[http.StatusOK] == 0 {
		t.Error("expected at least one successful stop")
	}
	// All responses should be either 200 or 409
	for code := range statusCodes {
		if code != http.StatusOK && code != http.StatusConflict {
			t.Errorf("unexpected status code in concurrent stops: %d", code)
		}
	}
}

// Test removed: GetStreamHandler replaced by WebSocket-based ChatStreamHandler

func TestStopStreamHandler_MaxLengthIDs(t *testing.T) {
	log := logger.New(logger.Config{Level: slog.LevelError})
	streamManager := streaming.NewStreamManager(nil, log)

	// Create IDs at exactly maxLength (should succeed)
	chatID := strings.Repeat("x", 256)
	messageID := strings.Repeat("y", 256)

	router := setupTestRouter(streamManager, log)

	req, _ := http.NewRequest("POST", "/api/v1/chats/"+chatID+"/messages/"+messageID+"/stop", nil)
	w := httptest.NewRecorder()

	router.ServeHTTP(w, req)

	// Should succeed with 404 (stream not found) not 400 (ID too long)
	if w.Code != http.StatusNotFound {
		t.Errorf("expected status 404 for valid-length IDs, got %d", w.Code)
	}
}
