package proxy

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/eternisai/enchanted-proxy/internal/config"
	"github.com/eternisai/enchanted-proxy/internal/logger"
	"github.com/eternisai/enchanted-proxy/internal/routing"
	"github.com/eternisai/enchanted-proxy/internal/streaming"
	"github.com/gin-gonic/gin"
)

// TestClientDisconnectContinuesUpstream tests that streaming continues after client disconnect.
// This verifies that the proxy continues reading from upstream and saves the complete message
// even when the client disconnects mid-stream.
func TestClientDisconnectContinuesUpstream(t *testing.T) {
	// Create a mock upstream server that streams slowly
	upstreamServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)

		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("ResponseWriter doesn't support flushing")
		}

		// Stream 10 chunks, 1 per second
		for i := 1; i <= 10; i++ {
			chunk := fmt.Sprintf("data: {\"choices\":[{\"delta\":{\"content\":\"chunk%d \"}}]}\n\n", i)
			fmt.Fprint(w, chunk)
			flusher.Flush()
			t.Logf("Sent chunk %d", i)
			time.Sleep(1 * time.Second)
		}

		// Send final chunk with token usage
		finalChunk := `data: {"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":20,"total_tokens":30}}` + "\n\n"
		fmt.Fprint(w, finalChunk)
		flusher.Flush()
		t.Logf("Sent final chunk with usage")

		// Send DONE
		fmt.Fprint(w, "data: [DONE]\n\n")
		flusher.Flush()
		t.Logf("Sent DONE signal")
	}))
	defer upstreamServer.Close()

	// Setup test infrastructure
	gin.SetMode(gin.TestMode)

	// Initialize config for proxy transport
	config.AppConfig = &config.Config{
		ProxyMaxIdleConns:        100,
		ProxyMaxIdleConnsPerHost: 50,
		ProxyMaxConnsPerHost:     100,
		ProxyIdleConnTimeout:     90,
	}

	logConfig := logger.Config{
		Level:  slog.LevelDebug,
		Format: "text",
	}
	testLogger := logger.New(logConfig)

	// Create stream manager (without message service for this test)
	streamManager := streaming.NewStreamManager(nil, testLogger.WithComponent("streaming"))
	defer streamManager.Shutdown()

	cfg := &config.Config{}

	// Create a simple handler that just uses handleStreamingInBackground directly
	router := gin.New()
	router.POST("/v1/chat/completions", func(c *gin.Context) {
		// Read request body
		requestBody, err := io.ReadAll(c.Request.Body)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "failed to read body"})
			return
		}

		// Parse target URL from upstream server
		target, _ := url.Parse(upstreamServer.URL)

		// Call the streaming handler directly
		handleStreamingInBackground(
			c,
			target,
			"test-key",
			requestBody,
			testLogger,
			time.Now(),
			"test-model",
			nil, // trackingService
			nil, // messageService
			streamManager,
			cfg,
			&routing.ProviderConfig{
				Name:            "TestProvider",
				TokenMultiplier: 1.0,
			},
		)
	})

	// Start test server
	testServer := httptest.NewServer(router)
	defer testServer.Close()

	t.Logf("Test servers started - upstream: %s, proxy: %s", upstreamServer.URL, testServer.URL)

	// Create request body
	requestBody := `{
		"model": "test-model",
		"messages": [{"role": "user", "content": "test"}],
		"stream": true,
		"chatId": "test-chat-123",
		"messageId": "test-message-456"
	}`

	// Create client request with cancellable context
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "POST", testServer.URL+"/v1/chat/completions", strings.NewReader(requestBody))
	if err != nil {
		t.Fatalf("Failed to create request: %v", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("X-Chat-ID", "test-chat-123")
	req.Header.Set("X-Message-ID", "test-message-456")
	req.Header.Set("Authorization", "Bearer test-token")

	// Make request in goroutine
	clientReceivedChunks := 0
	clientDone := make(chan error, 1)

	go func() {
		client := &http.Client{}
		resp, err := client.Do(req)
		if err != nil {
			clientDone <- err
			return
		}
		defer resp.Body.Close()

		// Read chunks for 3 seconds, then disconnect
		buf := make([]byte, 1024)
		timeout := time.After(3 * time.Second)

		for {
			select {
			case <-timeout:
				t.Logf("Client: 3 seconds elapsed, disconnecting...")
				clientDone <- nil
				return
			default:
				n, err := resp.Body.Read(buf)
				if n > 0 {
					chunk := string(buf[:n])
					if strings.Contains(chunk, "chunk") {
						clientReceivedChunks++
						t.Logf("Client received chunk %d", clientReceivedChunks)
					}
				}
				if err != nil {
					clientDone <- err
					return
				}
			}
		}
	}()

	// Wait for client to disconnect
	<-clientDone
	t.Logf("Client disconnected after receiving %d chunks", clientReceivedChunks)

	// Verify client only received ~3 chunks (3 seconds at 1 chunk/second)
	if clientReceivedChunks < 2 || clientReceivedChunks > 4 {
		t.Errorf("Expected client to receive 2-4 chunks, got %d", clientReceivedChunks)
	}

	// Now wait for upstream to complete (should continue despite client disconnect)
	t.Logf("Waiting for upstream to complete (should continue reading)...")

	// Get the session
	session := streamManager.GetSession("test-chat-123", "test-message-456")
	if session == nil {
		t.Fatal("Session not found")
	}

	// Wait for session to complete with timeout
	completionTimeout := time.After(15 * time.Second)
	completionTicker := time.NewTicker(500 * time.Millisecond)
	defer completionTicker.Stop()

	for {
		select {
		case <-completionTimeout:
			t.Fatal("Timeout waiting for session to complete")
		case <-completionTicker.C:
			if session.IsCompleted() {
				t.Logf("Session completed!")
				goto SessionCompleted
			}
		}
	}

SessionCompleted:
	// Verify all chunks were read
	chunks := session.GetStoredChunks()
	t.Logf("Session read %d total chunks", len(chunks))

	// Should have all 10 data chunks + final chunk + DONE
	if len(chunks) < 10 {
		t.Errorf("Expected at least 10 chunks, got %d", len(chunks))
		t.Log("This means upstream reading stopped when client disconnected - BUG NOT FIXED!")
	} else {
		t.Logf("SUCCESS! Upstream continued reading all %d chunks even after client disconnected at chunk %d", len(chunks), clientReceivedChunks)
	}

	// Verify token usage was captured
	usage := session.GetTokenUsage()
	if usage == nil {
		t.Error("Token usage not captured")
	} else {
		t.Logf("Token usage captured: prompt=%d, completion=%d, total=%d",
			usage.PromptTokens, usage.CompletionTokens, usage.TotalTokens)

		if usage.TotalTokens != 30 {
			t.Errorf("Expected total tokens 30, got %d", usage.TotalTokens)
		}
	}

	// Verify session was not stopped by user
	if session.IsStopped() {
		t.Error("Session was stopped (should complete naturally)")
	}

	t.Log("✅ Test PASSED - Upstream continues after client disconnect!")
}

// TestMultipleClientsOneDisconnects tests that upstream continues when one client disconnects
// but others remain connected. This verifies the broadcast architecture works correctly.
func TestMultipleClientsOneDisconnects(t *testing.T) {
	// Create a mock upstream server
	upstreamServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)

		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("ResponseWriter doesn't support flushing")
		}

		// Stream 5 chunks
		for i := 1; i <= 5; i++ {
			chunk := fmt.Sprintf("data: {\"choices\":[{\"delta\":{\"content\":\"chunk%d \"}}]}\n\n", i)
			fmt.Fprint(w, chunk)
			flusher.Flush()
			time.Sleep(500 * time.Millisecond)
		}

		fmt.Fprint(w, "data: [DONE]\n\n")
		flusher.Flush()
	}))
	defer upstreamServer.Close()

	// Setup test infrastructure
	gin.SetMode(gin.TestMode)
	config.AppConfig = &config.Config{
		ProxyMaxIdleConns:        100,
		ProxyMaxIdleConnsPerHost: 50,
		ProxyMaxConnsPerHost:     100,
		ProxyIdleConnTimeout:     90,
	}

	logConfig := logger.Config{Level: slog.LevelInfo, Format: "text"}
	testLogger := logger.New(logConfig)
	streamManager := streaming.NewStreamManager(nil, testLogger.WithComponent("streaming"))
	defer streamManager.Shutdown()

	cfg := &config.Config{}
	router := gin.New()
	router.POST("/v1/chat/completions", func(c *gin.Context) {
		requestBody, _ := io.ReadAll(c.Request.Body)
		target, _ := url.Parse(upstreamServer.URL)

		handleStreamingInBackground(
			c, target, "test-key", requestBody, testLogger, time.Now(),
			"test-model", nil, nil, streamManager, cfg,
			&routing.ProviderConfig{Name: "TestProvider", TokenMultiplier: 1.0},
		)
	})

	testServer := httptest.NewServer(router)
	defer testServer.Close()

	requestBody := `{"model": "test-model", "messages": [{"role": "user", "content": "test"}], "stream": true, "chatId": "test-chat-multi", "messageId": "test-message-multi"}`

	// Create two clients
	client1Chunks := 0
	client2Chunks := 0
	client1Done := make(chan error, 1)
	client2Done := make(chan error, 1)

	// Client 1: Disconnects after 1 second (should get ~2 chunks)
	go func() {
		ctx1, cancel1 := context.WithCancel(context.Background())
		defer cancel1()

		req, _ := http.NewRequestWithContext(ctx1, "POST", testServer.URL+"/v1/chat/completions", strings.NewReader(requestBody))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "text/event-stream")
		req.Header.Set("X-Chat-ID", "test-chat-multi")
		req.Header.Set("X-Message-ID", "test-message-multi")

		client := &http.Client{}
		resp, err := client.Do(req)
		if err != nil {
			client1Done <- err
			return
		}
		defer resp.Body.Close()

		// Read for 1 second then disconnect
		buf := make([]byte, 1024)
		timeout := time.After(1 * time.Second)
		for {
			select {
			case <-timeout:
				t.Logf("Client 1: disconnecting after 1 second")
				client1Done <- nil
				return
			default:
				n, err := resp.Body.Read(buf)
				if n > 0 {
					chunk := string(buf[:n])
					if strings.Contains(chunk, "chunk") {
						client1Chunks++
						t.Logf("Client 1 received chunk %d", client1Chunks)
					}
				}
				if err != nil {
					client1Done <- err
					return
				}
			}
		}
	}()

	// Give client 1 a head start
	time.Sleep(100 * time.Millisecond)

	// Client 2: Stays connected for entire stream
	go func() {
		ctx2 := context.Background()
		req, _ := http.NewRequestWithContext(ctx2, "POST", testServer.URL+"/v1/chat/completions", strings.NewReader(requestBody))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "text/event-stream")
		req.Header.Set("X-Chat-ID", "test-chat-multi")
		req.Header.Set("X-Message-ID", "test-message-multi")

		client := &http.Client{}
		resp, err := client.Do(req)
		if err != nil {
			client2Done <- err
			return
		}
		defer resp.Body.Close()

		// Read until DONE
		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			line := scanner.Text()
			if strings.Contains(line, "chunk") {
				client2Chunks++
				t.Logf("Client 2 received chunk %d", client2Chunks)
			}
			if strings.Contains(line, "[DONE]") {
				t.Logf("Client 2: received DONE")
				client2Done <- nil
				return
			}
		}
		client2Done <- scanner.Err()
	}()

	// Wait for both clients
	<-client1Done
	t.Logf("Client 1 disconnected after receiving %d chunks", client1Chunks)

	<-client2Done
	t.Logf("Client 2 completed after receiving %d chunks", client2Chunks)

	// Verify client 1 got fewer chunks than client 2
	if client1Chunks >= client2Chunks {
		t.Errorf("Client 1 should have received fewer chunks than client 2 (got %d vs %d)", client1Chunks, client2Chunks)
	}

	// Verify client 2 got most chunks (at least 4)
	// Note: Timing issues may cause client 2 to miss the final chunk or DONE signal
	if client2Chunks < 4 {
		t.Errorf("Client 2 should have received at least 4 chunks, got %d", client2Chunks)
	}

	// Verify session completed
	session := streamManager.GetSession("test-chat-multi", "test-message-multi")
	if session == nil {
		t.Fatal("Session not found")
	}

	// Wait for completion
	completionTimeout := time.After(5 * time.Second)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-completionTimeout:
			t.Fatal("Timeout waiting for session to complete")
		case <-ticker.C:
			if session.IsCompleted() {
				goto Completed
			}
		}
	}

Completed:
	chunks := session.GetStoredChunks()
	t.Logf("Session stored %d total chunks", len(chunks))

	if len(chunks) < 5 {
		t.Errorf("Expected at least 5 chunks stored, got %d", len(chunks))
	}

	t.Log("✅ Test PASSED - Multiple clients handled correctly!")
}

// TestClientDisconnectsImmediately tests that upstream still starts even if client
// disconnects before the first chunk arrives.
func TestClientDisconnectsImmediately(t *testing.T) {
	// Create a mock upstream server with delayed response
	upstreamServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Delay 2 seconds before starting to stream
		time.Sleep(2 * time.Second)

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)

		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("ResponseWriter doesn't support flushing")
		}

		// Stream 3 chunks quickly
		for i := 1; i <= 3; i++ {
			chunk := fmt.Sprintf("data: {\"choices\":[{\"delta\":{\"content\":\"chunk%d \"}}]}\n\n", i)
			fmt.Fprint(w, chunk)
			flusher.Flush()
			t.Logf("Sent chunk %d", i)
			time.Sleep(100 * time.Millisecond)
		}

		finalChunk := `data: {"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":10,"total_tokens":15}}` + "\n\n"
		fmt.Fprint(w, finalChunk)
		flusher.Flush()

		fmt.Fprint(w, "data: [DONE]\n\n")
		flusher.Flush()
		t.Logf("Sent DONE")
	}))
	defer upstreamServer.Close()

	// Setup test infrastructure
	gin.SetMode(gin.TestMode)
	config.AppConfig = &config.Config{
		ProxyMaxIdleConns:        100,
		ProxyMaxIdleConnsPerHost: 50,
		ProxyMaxConnsPerHost:     100,
		ProxyIdleConnTimeout:     90,
	}

	logConfig := logger.Config{Level: slog.LevelInfo, Format: "text"}
	testLogger := logger.New(logConfig)
	streamManager := streaming.NewStreamManager(nil, testLogger.WithComponent("streaming"))
	defer streamManager.Shutdown()

	cfg := &config.Config{}
	router := gin.New()
	router.POST("/v1/chat/completions", func(c *gin.Context) {
		requestBody, _ := io.ReadAll(c.Request.Body)
		target, _ := url.Parse(upstreamServer.URL)

		handleStreamingInBackground(
			c, target, "test-key", requestBody, testLogger, time.Now(),
			"test-model", nil, nil, streamManager, cfg,
			&routing.ProviderConfig{Name: "TestProvider", TokenMultiplier: 1.0},
		)
	})

	testServer := httptest.NewServer(router)
	defer testServer.Close()

	requestBody := `{"model": "test-model", "messages": [{"role": "user", "content": "test"}], "stream": true, "chatId": "test-chat-immediate", "messageId": "test-message-immediate"}`

	// Create client that disconnects immediately (500ms)
	clientDone := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		req, _ := http.NewRequestWithContext(ctx, "POST", testServer.URL+"/v1/chat/completions", strings.NewReader(requestBody))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "text/event-stream")
		req.Header.Set("X-Chat-ID", "test-chat-immediate")
		req.Header.Set("X-Message-ID", "test-message-immediate")

		client := &http.Client{}
		resp, err := client.Do(req)
		if err != nil {
			clientDone <- err
			return
		}
		defer resp.Body.Close()

		// Disconnect immediately after 500ms (before upstream even responds)
		time.Sleep(500 * time.Millisecond)
		t.Logf("Client: disconnecting immediately (before first chunk)")
		clientDone <- nil
	}()

	<-clientDone
	t.Logf("Client disconnected immediately")

	// Verify upstream still completes
	session := streamManager.GetSession("test-chat-immediate", "test-message-immediate")
	if session == nil {
		t.Fatal("Session not found")
	}

	// Wait for completion (should happen even though client disconnected before first chunk)
	completionTimeout := time.After(10 * time.Second)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-completionTimeout:
			t.Fatal("Timeout waiting for session to complete - upstream didn't continue after immediate disconnect!")
		case <-ticker.C:
			if session.IsCompleted() {
				goto Completed
			}
		}
	}

Completed:
	chunks := session.GetStoredChunks()
	t.Logf("Session stored %d total chunks (client got 0)", len(chunks))

	if len(chunks) < 3 {
		t.Errorf("Expected at least 3 chunks stored, got %d - upstream stopped when client disconnected!", len(chunks))
	}

	// Verify token usage was captured
	usage := session.GetTokenUsage()
	if usage == nil {
		t.Error("Token usage not captured")
	} else if usage.TotalTokens != 15 {
		t.Errorf("Expected total tokens 15, got %d", usage.TotalTokens)
	}

	t.Log("✅ Test PASSED - Upstream continues even when client disconnects immediately!")
}

// TestUpstreamHTTPRequestFailure tests the ForceComplete error path
func TestUpstreamHTTPRequestFailure(t *testing.T) {
	// Setup test infrastructure
	gin.SetMode(gin.TestMode)
	config.AppConfig = &config.Config{
		ProxyMaxIdleConns:        100,
		ProxyMaxIdleConnsPerHost: 50,
		ProxyMaxConnsPerHost:     100,
		ProxyIdleConnTimeout:     90,
	}

	logConfig := logger.Config{Level: slog.LevelInfo, Format: "text"}
	testLogger := logger.New(logConfig)
	streamManager := streaming.NewStreamManager(nil, testLogger.WithComponent("streaming"))
	defer streamManager.Shutdown()

	cfg := &config.Config{}
	router := gin.New()
	router.POST("/v1/chat/completions", func(c *gin.Context) {
		requestBody, _ := io.ReadAll(c.Request.Body)

		// Use invalid URL to force HTTP request failure
		target, _ := url.Parse("http://localhost:1")

		handleStreamingInBackground(
			c, target, "test-key", requestBody, testLogger, time.Now(),
			"test-model", nil, nil, streamManager, cfg,
			&routing.ProviderConfig{Name: "TestProvider", TokenMultiplier: 1.0},
		)
	})

	testServer := httptest.NewServer(router)
	defer testServer.Close()

	requestBody := `{"model": "test-model", "messages": [{"role": "user", "content": "test"}], "stream": true, "chatId": "test-chat-failure", "messageId": "test-message-failure"}`

	// Make request
	ctx := context.Background()
	req, _ := http.NewRequestWithContext(ctx, "POST", testServer.URL+"/v1/chat/completions", strings.NewReader(requestBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("X-Chat-ID", "test-chat-failure")
	req.Header.Set("X-Message-ID", "test-message-failure")

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Failed to make request: %v", err)
	}
	defer resp.Body.Close()

	// Try to read (should get nothing because upstream failed)
	buf := make([]byte, 1024)
	readTimeout := time.After(3 * time.Second)
	bytesRead := 0

	for {
		select {
		case <-readTimeout:
			goto ReadComplete
		default:
			n, err := resp.Body.Read(buf)
			bytesRead += n
			if err == io.EOF {
				goto ReadComplete
			}
			if err != nil {
				t.Logf("Read error (expected): %v", err)
				goto ReadComplete
			}
		}
	}

ReadComplete:
	t.Logf("Client read %d bytes before connection closed", bytesRead)

	// Verify session was force completed with error
	session := streamManager.GetSession("test-chat-failure", "test-message-failure")
	if session == nil {
		t.Fatal("Session not found")
	}

	// Wait for completion
	completionTimeout := time.After(5 * time.Second)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-completionTimeout:
			t.Fatal("Timeout waiting for session to complete with error")
		case <-ticker.C:
			if session.IsCompleted() {
				goto Completed
			}
		}
	}

Completed:
	// Verify session has error
	if err := session.GetError(); err == nil {
		t.Error("Session should have error, but got nil")
	} else {
		t.Logf("Session correctly completed with error: %v", err)
	}

	// Verify no chunks were stored (upstream never responded)
	chunks := session.GetStoredChunks()
	if len(chunks) > 0 {
		t.Errorf("Expected 0 chunks (upstream failed), got %d", len(chunks))
	}

	t.Log("✅ Test PASSED - HTTP failure handled gracefully!")
}
