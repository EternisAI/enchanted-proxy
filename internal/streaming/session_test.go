package streaming

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/eternisai/enchanted-proxy/internal/logger"
)

// mockReadCloser implements io.ReadCloser for testing
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

// slowMockReadCloser implements io.ReadCloser with delays between lines
type slowMockReadCloser struct {
	lines  []string
	index  int
	buffer []byte
	delay  time.Duration
	closed bool
}

func (s *slowMockReadCloser) Read(p []byte) (n int, err error) {
	// If we have buffered data, return it first
	if len(s.buffer) > 0 {
		n = copy(p, s.buffer)
		s.buffer = s.buffer[n:]
		return n, nil
	}

	// Check if we're done
	if s.index >= len(s.lines) {
		return 0, io.EOF
	}

	// Add delay between lines to simulate slow streaming
	if s.index > 0 {
		time.Sleep(s.delay)
	}

	// Get next line
	line := s.lines[s.index] + "\n"
	s.index++

	// Store in buffer and return what fits
	s.buffer = []byte(line)
	n = copy(p, s.buffer)
	s.buffer = s.buffer[n:]
	return n, nil
}

func (s *slowMockReadCloser) Close() error {
	s.closed = true
	return nil
}

func newSlowMockSSEStream(lines []string, delayPerLine time.Duration) io.ReadCloser {
	return &slowMockReadCloser{
		lines: lines,
		delay: delayPerLine,
	}
}

func TestNewStreamSession(t *testing.T) {
	log := logger.New(logger.Config{Level: slog.LevelInfo})
	body := newMockSSEStream([]string{"data: test"})

	session := NewStreamSession("chat-123", "msg-456", body, log)

	if session == nil {
		t.Fatal("NewStreamSession returned nil")
	}
	if session.chatID != "chat-123" {
		t.Errorf("expected chatID 'chat-123', got %s", session.chatID)
	}
	if session.messageID != "msg-456" {
		t.Errorf("expected messageID 'msg-456', got %s", session.messageID)
	}
	if session.startTime.IsZero() {
		t.Error("startTime not set")
	}
	if session.subscribers == nil {
		t.Error("subscribers map is nil")
	}
	if session.chunks == nil {
		t.Error("chunks slice is nil")
	}
}

func TestStreamSessionSubscribe(t *testing.T) {
	log := logger.New(logger.Config{Level: slog.LevelInfo})
	body := newMockSSEStream([]string{"data: test"})
	session := NewStreamSession("chat-123", "msg-456", body, log)

	ctx := context.Background()
	opts := DefaultSubscriberOptions()

	sub, err := session.Subscribe(ctx, "sub-1", opts)
	if err != nil {
		t.Fatalf("Subscribe failed: %v", err)
	}
	if sub == nil {
		t.Fatal("Subscribe returned nil subscriber")
	}
	if sub.ID != "sub-1" {
		t.Errorf("expected subscriber ID 'sub-1', got %s", sub.ID)
	}

	// Check subscriber count
	if session.GetSubscriberCount() != 1 {
		t.Errorf("expected 1 subscriber, got %d", session.GetSubscriberCount())
	}
}

func TestStreamSessionUnsubscribe(t *testing.T) {
	log := logger.New(logger.Config{Level: slog.LevelInfo})
	body := newMockSSEStream([]string{"data: test"})
	session := NewStreamSession("chat-123", "msg-456", body, log)

	ctx := context.Background()
	opts := DefaultSubscriberOptions()

	sub, _ := session.Subscribe(ctx, "sub-1", opts)
	if session.GetSubscriberCount() != 1 {
		t.Error("expected 1 subscriber after subscribe")
	}

	session.Unsubscribe(sub.ID)
	if session.GetSubscriberCount() != 0 {
		t.Error("expected 0 subscribers after unsubscribe")
	}

	// Unsubscribe again (should be safe)
	session.Unsubscribe(sub.ID)
}

func TestStreamSessionBasicStreaming(t *testing.T) {
	log := logger.New(logger.Config{Level: slog.LevelError}) // Reduce noise in tests
	lines := []string{
		"data: {\"choices\":[{\"delta\":{\"content\":\"Hello\"}}]}",
		"data: {\"choices\":[{\"delta\":{\"content\":\" World\"}}]}",
		"data: [DONE]",
	}
	body := newMockSSEStream(lines)
	session := NewStreamSession("chat-123", "msg-456", body, log)

	// Start the session
	session.Start()

	// Subscribe
	ctx := context.Background()
	opts := DefaultSubscriberOptions()
	sub, err := session.Subscribe(ctx, "sub-1", opts)
	if err != nil {
		t.Fatalf("Subscribe failed: %v", err)
	}

	// Collect chunks
	var receivedChunks []StreamChunk
	timeout := time.After(2 * time.Second)

collectLoop:
	for {
		select {
		case chunk, ok := <-sub.Ch:
			if !ok {
				break collectLoop
			}
			receivedChunks = append(receivedChunks, chunk)
			if chunk.IsFinal {
				break collectLoop
			}
		case <-timeout:
			t.Fatal("timeout waiting for chunks")
		}
	}

	// Verify we got chunks
	if len(receivedChunks) == 0 {
		t.Fatal("no chunks received")
	}

	// Wait for completion
	time.Sleep(100 * time.Millisecond)
	if !session.IsCompleted() {
		t.Error("session should be completed")
	}

	// Check content extraction
	content := session.GetContent()
	if content != "Hello World" {
		t.Errorf("expected content 'Hello World', got '%s'", content)
	}
}

func TestStreamSessionMultipleSubscribers(t *testing.T) {
	log := logger.New(logger.Config{Level: slog.LevelError})
	lines := []string{
		"data: {\"choices\":[{\"delta\":{\"content\":\"Test\"}}]}",
		"data: [DONE]",
	}
	body := newMockSSEStream(lines)
	session := NewStreamSession("chat-123", "msg-456", body, log)

	session.Start()

	// Subscribe two clients
	ctx := context.Background()
	opts := DefaultSubscriberOptions()

	sub1, _ := session.Subscribe(ctx, "sub-1", opts)
	sub2, _ := session.Subscribe(ctx, "sub-2", opts)

	if session.GetSubscriberCount() != 2 {
		t.Errorf("expected 2 subscribers, got %d", session.GetSubscriberCount())
	}

	// Both should receive chunks
	var received1, received2 bool
	timeout := time.After(2 * time.Second)

	done := make(chan bool, 2)

	go func() {
		for chunk := range sub1.Ch {
			if strings.Contains(chunk.Line, "Test") {
				received1 = true
			}
			if chunk.IsFinal {
				break
			}
		}
		done <- true
	}()

	go func() {
		for chunk := range sub2.Ch {
			if strings.Contains(chunk.Line, "Test") {
				received2 = true
			}
			if chunk.IsFinal {
				break
			}
		}
		done <- true
	}()

	// Wait for both subscribers
	for i := 0; i < 2; i++ {
		select {
		case <-done:
		case <-timeout:
			t.Fatal("timeout waiting for subscribers")
		}
	}

	if !received1 {
		t.Error("subscriber 1 did not receive content")
	}
	if !received2 {
		t.Error("subscriber 2 did not receive content")
	}
}

func TestStreamSessionStop(t *testing.T) {
	log := logger.New(logger.Config{Level: slog.LevelError})
	// Create a slow streaming source that takes time to complete
	// This allows us to call Stop() while the stream is still active
	longContent := make([]string, 100)
	for i := range longContent {
		longContent[i] = "data: {\"choices\":[{\"delta\":{\"content\":\"test\"}}]}"
	}
	longContent = append(longContent, "data: [DONE]")

	// Use slow mock with 5ms delay per line (100 lines * 5ms = 500ms total)
	body := newSlowMockSSEStream(longContent, 5*time.Millisecond)
	session := NewStreamSession("chat-123", "msg-456", body, log)

	session.Start()

	// Subscribe
	ctx := context.Background()
	opts := DefaultSubscriberOptions()
	sub, _ := session.Subscribe(ctx, "sub-1", opts)

	// Start reading chunks in background
	var gotStopEvent bool
	done := make(chan bool)
	go func() {
		for chunk := range sub.Ch {
			if strings.Contains(chunk.Line, "stream_stopped") {
				gotStopEvent = true
			}
			if chunk.IsFinal {
				break
			}
		}
		done <- true
	}()

	// Wait for some chunks to be processed, then stop
	time.Sleep(50 * time.Millisecond)

	err := session.Stop("user-123", StopReasonUserCancelled)
	if err != nil {
		t.Fatalf("Stop failed: %v", err)
	}

	if !session.IsStopped() {
		t.Error("session should be stopped")
	}

	stoppedBy, reason := session.GetStopInfo()
	if stoppedBy != "user-123" {
		t.Errorf("expected stoppedBy 'user-123', got %s", stoppedBy)
	}
	if reason != StopReasonUserCancelled {
		t.Errorf("expected reason UserCancelled, got %s", reason)
	}

	// Wait for chunk reader to finish
	select {
	case <-done:
		// Success
	case <-time.After(1 * time.Second):
		t.Fatal("timeout waiting for chunks")
	}

	// Should be completed
	if !session.IsCompleted() {
		t.Error("session should be completed after stop")
	}

	// Check that we got a stop event
	if !gotStopEvent {
		t.Error("did not receive stop event")
	}
}

func TestStreamSessionLateJoiner(t *testing.T) {
	log := logger.New(logger.Config{Level: slog.LevelError})
	lines := []string{
		"data: {\"choices\":[{\"delta\":{\"content\":\"Hello\"}}]}",
		"data: {\"choices\":[{\"delta\":{\"content\":\" World\"}}]}",
		"data: [DONE]",
	}
	body := newMockSSEStream(lines)
	session := NewStreamSession("chat-123", "msg-456", body, log)

	session.Start()

	// First subscriber
	ctx := context.Background()
	opts := DefaultSubscriberOptions()
	sub1, _ := session.Subscribe(ctx, "sub-1", opts)

	// Wait for stream to complete
	for chunk := range sub1.Ch {
		if chunk.IsFinal {
			break
		}
	}

	time.Sleep(100 * time.Millisecond)

	// Second subscriber joins after completion (late joiner)
	opts2 := SubscriberOptions{
		ReplayFromStart: true,
		BufferSize:      100,
	}
	sub2, _ := session.Subscribe(ctx, "sub-2", opts2)

	// Late joiner should get all buffered chunks immediately
	var receivedChunks []StreamChunk
	timeout := time.After(1 * time.Second)

collectLoop:
	for {
		select {
		case chunk, ok := <-sub2.Ch:
			if !ok {
				break collectLoop
			}
			receivedChunks = append(receivedChunks, chunk)
			if chunk.IsFinal {
				break collectLoop
			}
		case <-timeout:
			break collectLoop
		}
	}

	if len(receivedChunks) == 0 {
		t.Fatal("late joiner received no chunks")
	}

	// Verify content
	foundHello := false
	foundWorld := false
	for _, chunk := range receivedChunks {
		if strings.Contains(chunk.Line, "Hello") {
			foundHello = true
		}
		if strings.Contains(chunk.Line, "World") {
			foundWorld = true
		}
	}

	if !foundHello || !foundWorld {
		t.Error("late joiner did not receive all content")
	}
}

func TestStreamSessionGetInfo(t *testing.T) {
	log := logger.New(logger.Config{Level: slog.LevelError})
	body := newMockSSEStream([]string{"data: test"})
	session := NewStreamSession("chat-123", "msg-456", body, log)

	info := session.GetInfo()
	if info.ChatID != "chat-123" {
		t.Errorf("expected chatID 'chat-123', got %s", info.ChatID)
	}
	if info.MessageID != "msg-456" {
		t.Errorf("expected messageID 'msg-456', got %s", info.MessageID)
	}
	if info.SessionKey != "chat-123:msg-456" {
		t.Errorf("expected sessionKey 'chat-123:msg-456', got %s", info.SessionKey)
	}
	if info.Completed {
		t.Error("session should not be completed yet")
	}
}

func TestStreamSessionGetStoredChunks(t *testing.T) {
	log := logger.New(logger.Config{Level: slog.LevelError})
	lines := []string{
		"data: chunk1",
		"data: chunk2",
		"data: [DONE]",
	}
	body := newMockSSEStream(lines)
	session := NewStreamSession("chat-123", "msg-456", body, log)

	session.Start()

	// Wait for completion
	time.Sleep(200 * time.Millisecond)

	chunks := session.GetStoredChunks()
	if len(chunks) == 0 {
		t.Error("no chunks stored")
	}

	// Verify it's a copy (modifying returned slice shouldn't affect session)
	originalLen := len(chunks)
	chunks = append(chunks, StreamChunk{Index: 999})
	sessionChunks := session.GetStoredChunks()
	if len(sessionChunks) != originalLen {
		t.Error("GetStoredChunks should return a copy")
	}
}
