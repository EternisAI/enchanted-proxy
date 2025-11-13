package proxy

import (
	"context"
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
				messages.GET("/:messageId/stream", GetStreamHandler(log, streamManager, nil))
				messages.POST("/:messageId/stop", StopStreamHandler(log, streamManager, nil))
				messages.GET("/:messageId/status", StreamStatusHandler(log, streamManager, nil))
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

func TestGetStreamHandler_Success(t *testing.T) {
	log := logger.New(logger.Config{Level: slog.LevelError})
	streamManager := streaming.NewStreamManager(nil, log)

	// Create a stream with some content
	lines := []string{
		"data: {\"choices\":[{\"delta\":{\"content\":\"Hello\"}}]}",
		"data: {\"choices\":[{\"delta\":{\"content\":\" World\"}}]}",
		"data: [DONE]",
	}
	body := newMockSSEStream(lines)
	_, _ = streamManager.GetOrCreateSession("chat-123", "msg-456", body)

	// Wait for stream to complete
	time.Sleep(100 * time.Millisecond)

	router := setupTestRouter(streamManager, log)

	// Subscribe via GET endpoint
	req := httptest.NewRequest("GET", "/api/v1/chats/chat-123/messages/msg-456/stream", nil)
	req.Header.Set("Accept", "text/event-stream")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", w.Code)
	}

	// Check SSE headers
	contentType := w.Header().Get("Content-Type")
	if contentType != "text/event-stream" {
		t.Errorf("expected Content-Type 'text/event-stream', got %s", contentType)
	}

	cacheControl := w.Header().Get("Cache-Control")
	if cacheControl != "no-cache" {
		t.Errorf("expected Cache-Control 'no-cache', got %s", cacheControl)
	}

	// Check response body contains data
	body_str := w.Body.String()
	if !strings.Contains(body_str, "Hello") {
		t.Error("response should contain 'Hello'")
	}
	if !strings.Contains(body_str, "World") {
		t.Error("response should contain 'World'")
	}
	if !strings.Contains(body_str, "[DONE]") {
		t.Error("response should contain '[DONE]'")
	}
}

func TestGetStreamHandler_NotFound(t *testing.T) {
	log := logger.New(logger.Config{Level: slog.LevelError})
	streamManager := streaming.NewStreamManager(nil, log)
	router := setupTestRouter(streamManager, log)

	// Try to subscribe to non-existent stream
	req := httptest.NewRequest("GET", "/api/v1/chats/chat-123/messages/msg-999/stream", nil)
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

func TestGetStreamHandler_WithReplay(t *testing.T) {
	log := logger.New(logger.Config{Level: slog.LevelError})
	streamManager := streaming.NewStreamManager(nil, log)

	// Create a stream
	lines := []string{
		"data: {\"choices\":[{\"delta\":{\"content\":\"Chunk1\"}}]}",
		"data: {\"choices\":[{\"delta\":{\"content\":\"Chunk2\"}}]}",
		"data: [DONE]",
	}
	body := newMockSSEStream(lines)
	_, _ = streamManager.GetOrCreateSession("chat-123", "msg-456", body)

	// Wait for completion
	time.Sleep(100 * time.Millisecond)

	router := setupTestRouter(streamManager, log)

	// Subscribe with replay=true
	req := httptest.NewRequest("GET", "/api/v1/chats/chat-123/messages/msg-456/stream?replay=true", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", w.Code)
	}

	body_str := w.Body.String()
	if !strings.Contains(body_str, "Chunk1") {
		t.Error("replay should contain Chunk1")
	}
	if !strings.Contains(body_str, "Chunk2") {
		t.Error("replay should contain Chunk2")
	}
}

func TestGetStreamHandler_WaitForCompletion(t *testing.T) {
	log := logger.New(logger.Config{Level: slog.LevelError})
	streamManager := streaming.NewStreamManager(nil, log)
	router := setupTestRouter(streamManager, log)

	// Start request with wait_for_completion=true in background
	done := make(chan bool)
	var statusCode int

	go func() {
		req := httptest.NewRequest("GET", "/api/v1/chats/chat-123/messages/msg-456/stream?wait_for_completion=true", nil)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		statusCode = w.Code
		done <- true
	}()

	// Create stream after 500ms
	time.Sleep(500 * time.Millisecond)
	lines := []string{
		"data: {\"choices\":[{\"delta\":{\"content\":\"Hello\"}}]}",
		"data: [DONE]",
	}
	body := newMockSSEStream(lines)
	_, _ = streamManager.GetOrCreateSession("chat-123", "msg-456", body)

	// Wait for request to complete
	select {
	case <-done:
		if statusCode != http.StatusOK {
			t.Errorf("expected status 200, got %d", statusCode)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for request")
	}
}

func TestGetStreamHandler_WaitTimeout(t *testing.T) {
	log := logger.New(logger.Config{Level: slog.LevelError})
	streamManager := streaming.NewStreamManager(nil, log)
	router := setupTestRouter(streamManager, log)

	// Request with wait but never create stream
	req := httptest.NewRequest("GET", "/api/v1/chats/chat-123/messages/msg-999/stream?wait_for_completion=true", nil)

	// Use context with short timeout to speed up test
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	req = req.WithContext(ctx)

	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected status 404, got %d", w.Code)
	}
}

func TestStreamStatusHandler_Success(t *testing.T) {
	log := logger.New(logger.Config{Level: slog.LevelError})
	streamManager := streaming.NewStreamManager(nil, log)

	// Create a stream
	lines := []string{
		"data: {\"choices\":[{\"delta\":{\"content\":\"test\"}}]}",
		"data: [DONE]",
	}
	body := newMockSSEStream(lines)
	session, _ := streamManager.GetOrCreateSession("chat-123", "msg-456", body)

	// Subscribe a client
	ctx := context.Background()
	opts := streaming.DefaultSubscriberOptions()
	session.Subscribe(ctx, "sub-1", opts)

	router := setupTestRouter(streamManager, log)

	// Get status
	req := httptest.NewRequest("GET", "/api/v1/chats/chat-123/messages/msg-456/status", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", w.Code)
	}

	var response map[string]interface{}
	err := json.Unmarshal(w.Body.Bytes(), &response)
	if err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}

	if !response["exists"].(bool) {
		t.Error("expected exists to be true")
	}
	if response["message_id"].(string) != "msg-456" {
		t.Errorf("expected message_id 'msg-456', got %s", response["message_id"])
	}
	if response["chat_id"].(string) != "chat-123" {
		t.Errorf("expected chat_id 'chat-123', got %s", response["chat_id"])
	}
	if response["subscriber_count"].(float64) != 1 {
		t.Errorf("expected subscriber_count 1, got %v", response["subscriber_count"])
	}
}

func TestStreamStatusHandler_NotFound(t *testing.T) {
	log := logger.New(logger.Config{Level: slog.LevelError})
	streamManager := streaming.NewStreamManager(nil, log)
	router := setupTestRouter(streamManager, log)

	// Get status of non-existent stream
	req := httptest.NewRequest("GET", "/api/v1/chats/chat-123/messages/msg-999/status", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected status 404, got %d", w.Code)
	}

	var response map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &response)
	if response["exists"] != false {
		t.Error("expected exists to be false")
	}
}

func TestStreamStatusHandler_StoppedStream(t *testing.T) {
	log := logger.New(logger.Config{Level: slog.LevelError})
	streamManager := streaming.NewStreamManager(nil, log)

	// Create a very slow stream that won't complete quickly
	lines := make([]string, 50) // More lines
	for i := range lines {
		lines[i] = "data: {\"choices\":[{\"delta\":{\"content\":\"test\"}}]}"
	}
	lines = append(lines, "data: [DONE]")
	body := newSlowMockSSEStream(lines, 200*time.Millisecond) // Slower delay
	session, _ := streamManager.GetOrCreateSession("chat-123", "msg-456", body)

	// Wait a bit then stop it (stream should still be running)
	time.Sleep(400 * time.Millisecond) // Wait for 2 lines
	session.Stop("test-user", streaming.StopReasonUserCancelled)

	router := setupTestRouter(streamManager, log)

	// Get status
	req := httptest.NewRequest("GET", "/api/v1/chats/chat-123/messages/msg-456/status", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	var response map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &response)

	// Debug: print response if test fails
	if response["stopped"] == nil {
		t.Logf("Response body: %s", w.Body.String())
		t.Log("Session stopped:", session.IsStopped())
		t.Log("Session completed:", session.IsCompleted())
	}

	stopped, ok := response["stopped"].(bool)
	if !ok || !stopped {
		t.Error("expected stopped to be true")
	}
	stoppedBy, ok := response["stopped_by"].(string)
	if !ok || stoppedBy != "test-user" {
		t.Errorf("expected stopped_by 'test-user', got %v", response["stopped_by"])
	}
	stopReason, ok := response["stop_reason"].(string)
	if !ok || stopReason != "user_cancelled" {
		t.Errorf("expected stop_reason 'user_cancelled', got %v", response["stop_reason"])
	}
}

func TestStreamStatusHandler_CompletedStream(t *testing.T) {
	log := logger.New(logger.Config{Level: slog.LevelError})
	streamManager := streaming.NewStreamManager(nil, log)

	// Create a stream that completes
	lines := []string{
		"data: {\"choices\":[{\"delta\":{\"content\":\"test\"}}]}",
		"data: [DONE]",
	}
	body := newMockSSEStream(lines)
	_, _ = streamManager.GetOrCreateSession("chat-123", "msg-456", body)

	// Wait for completion
	time.Sleep(100 * time.Millisecond)

	router := setupTestRouter(streamManager, log)

	// Get status
	req := httptest.NewRequest("GET", "/api/v1/chats/chat-123/messages/msg-456/status", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	var response map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &response)

	completed, ok := response["completed"].(bool)
	if !ok || !completed {
		t.Error("expected completed to be true")
	}
	if _, ok := response["duration_ms"]; !ok {
		t.Error("expected duration_ms in response for completed stream")
	}
}

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

func TestGetStreamHandler_IDTooLong(t *testing.T) {
	log := logger.New(logger.Config{Level: slog.LevelError})
	streamManager := streaming.NewStreamManager(nil, log)
	router := setupTestRouter(streamManager, log)

	// Create a message ID that exceeds maxMessageIDLength (256)
	longMessageID := strings.Repeat("b", 257)

	req, _ := http.NewRequest("GET", "/api/v1/chats/chat-123/messages/"+longMessageID+"/stream", nil)
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

func TestStreamStatusHandler_IDTooLong(t *testing.T) {
	log := logger.New(logger.Config{Level: slog.LevelError})
	streamManager := streaming.NewStreamManager(nil, log)
	router := setupTestRouter(streamManager, log)

	// Both IDs too long
	longChatID := strings.Repeat("c", 257)
	longMessageID := strings.Repeat("d", 257)

	req, _ := http.NewRequest("GET", "/api/v1/chats/"+longChatID+"/messages/"+longMessageID+"/status", nil)
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

func TestGetStreamHandler_Unauthenticated(t *testing.T) {
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
				messages.GET("/:messageId/stream", GetStreamHandler(log, streamManager, nil))
			}
		}
	}

	req, _ := http.NewRequest("GET", "/api/v1/chats/chat-123/messages/msg-456/stream", nil)
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

func TestStreamStatusHandler_Unauthenticated(t *testing.T) {
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
				messages.GET("/:messageId/status", StreamStatusHandler(log, streamManager, nil))
			}
		}
	}

	req, _ := http.NewRequest("GET", "/api/v1/chats/chat-123/messages/msg-456/status", nil)
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

func TestGetStreamHandler_EmptyStream(t *testing.T) {
	log := logger.New(logger.Config{Level: slog.LevelError})
	streamManager := streaming.NewStreamManager(nil, log)

	// Create a stream with only [DONE]
	lines := []string{"data: [DONE]"}
	body := newMockSSEStream(lines)

	// Start session
	_, _ = streamManager.GetOrCreateSession("chat-empty", "msg-empty", body)
	time.Sleep(100 * time.Millisecond) // Wait for stream to complete

	router := setupTestRouter(streamManager, log)

	// Subscribe to the completed empty stream
	req, _ := http.NewRequest("GET", "/api/v1/chats/chat-empty/messages/msg-empty/stream?replay=true", nil)
	w := httptest.NewRecorder()

	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", w.Code)
	}

	// Should still receive the [DONE] message
	body_str := w.Body.String()
	if !strings.Contains(body_str, "[DONE]") {
		t.Error("expected [DONE] message in response")
	}
}

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
