package proxy

import (
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

// TestClientDisconnectContinuesUpstream tests that streaming continues after client disconnect
func TestClientDisconnectContinuesUpstream(t *testing.T) {
	// Create a mock upstream server that streams slowly
	totalChunks := 0
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
			totalChunks++
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
