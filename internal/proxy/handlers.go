package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/eternisai/enchanted-proxy/internal/auth"
	"github.com/eternisai/enchanted-proxy/internal/background"
	"github.com/eternisai/enchanted-proxy/internal/config"
	"github.com/eternisai/enchanted-proxy/internal/logger"
	"github.com/eternisai/enchanted-proxy/internal/messaging"
	"github.com/eternisai/enchanted-proxy/internal/request_tracking"
	"github.com/eternisai/enchanted-proxy/internal/routing"
	"github.com/eternisai/enchanted-proxy/internal/streaming"
	"github.com/eternisai/enchanted-proxy/internal/title_generation"
	"github.com/eternisai/enchanted-proxy/internal/tools"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

var (
	proxyTransport *http.Transport
	transportOnce  sync.Once
)

func initProxyTransport() {
	transportOnce.Do(func() {
		// Adds connection pooling.
		proxyTransport = &http.Transport{
			MaxIdleConns:        config.AppConfig.ProxyMaxIdleConns,
			MaxIdleConnsPerHost: config.AppConfig.ProxyMaxIdleConnsPerHost,
			MaxConnsPerHost:     config.AppConfig.ProxyMaxConnsPerHost,
			IdleConnTimeout:     time.Duration(config.AppConfig.ProxyIdleConnTimeout) * time.Second,
			DisableKeepAlives:   false,
			DisableCompression:  true,
			ForceAttemptHTTP2:   true,
			DialContext: (&net.Dialer{
				Timeout:   30 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			TLSHandshakeTimeout:   30 * time.Second,
			ResponseHeaderTimeout: 120 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
		}
	})
}

func createReverseProxyWithPooling(target *url.URL) *httputil.ReverseProxy {
	// Runs only once.
	initProxyTransport()
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.Transport = proxyTransport
	return proxy
}

func ProxyHandler(
	logger *logger.Logger,
	trackingService *request_tracking.Service,
	messageService *messaging.Service,
	titleService *title_generation.Service,
	streamManager *streaming.StreamManager,
	pollingManager *background.PollingManager,
	modelRouter *routing.ModelRouter,
	toolRegistry *tools.Registry,
	cfg *config.Config,
) gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		log := logger.WithContext(c.Request.Context()).WithComponent("proxy")

		var (
			requestBody []byte
			err         error
			model       string
		)

		if c.Request.Body != nil {
			requestBody, err = io.ReadAll(c.Request.Body)
			if err != nil {
				log.Error("failed to read request body", slog.String("error", err.Error()))
				c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to read request body"})
				return
			}
			c.Request.Body = io.NopCloser(bytes.NewReader(requestBody))

			model = ExtractModelFromRequestBody(c.Request.URL.Path, requestBody)

			// Extract chatId and messageId from request body for session tracking
			// Store in context so handlers can access them as fallback if headers are missing
			var reqBody map[string]interface{}
			if err := json.Unmarshal(requestBody, &reqBody); err == nil {
				if chatID, ok := reqBody["chatId"].(string); ok && chatID != "" {
					c.Set("bodyChatId", chatID)
				}
				if messageID, ok := reqBody["messageId"].(string); ok && messageID != "" {
					c.Set("bodyMessageId", messageID)
				}
			}
		}

		// Get client platform for routing
		platform := c.GetHeader("X-Client-Platform")
		if platform == "" {
			platform = "mobile" // Default to mobile
		}

		// Route based on model ID (X-BASE-URL header is ignored for security)
		// Model field is required - proxy controls all routing
		if model == "" {
			log.Warn("missing model field in request body")
			c.JSON(http.StatusBadRequest, gin.H{"error": "Model field is required"})
			return
		}

		if modelRouter == nil {
			log.Error("model router not initialized")
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Routing service unavailable"})
			return
		}

		// Route model to provider
		provider, err := modelRouter.RouteModel(model, platform)
		if err != nil {
			log.Error("failed to route model",
				slog.String("error", err.Error()),
				slog.String("model", model))
			c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("No provider configured for model: %s", model)})
			return
		}

		baseURL := provider.BaseURL
		apiKey := provider.APIKey

		// Warn if client sent X-BASE-URL (deprecated, ignored)
		if legacyBaseURL := c.GetHeader("X-BASE-URL"); legacyBaseURL != "" {
			log.Warn("X-BASE-URL header is deprecated and ignored - routing based on model",
				slog.String("x_base_url", legacyBaseURL),
				slog.String("model", model),
				slog.String("actual_provider", provider.Name),
				slog.String("actual_base_url", baseURL))
		}

		log.Info("routed model to provider",
			slog.String("model", model),
			slog.String("provider", provider.Name),
			slog.String("base_url", baseURL),
			slog.String("api_type", string(provider.APIType)),
			slog.Float64("multiplier", provider.TokenMultiplier))

		// For Eternis models (GLM, Dolphin): Add stream_options to enable usage reporting
		// vLLM and similar servers need explicit flag to include usage in streaming responses
		if provider.Name == "Eternis" && len(requestBody) > 0 {
			var reqBody map[string]interface{}
			if err := json.Unmarshal(requestBody, &reqBody); err == nil {
				// Only add for streaming requests
				if stream, ok := reqBody["stream"].(bool); ok && stream {
					reqBody["stream_options"] = map[string]interface{}{
						"include_usage": true,
					}
					// Re-serialize request body
					if modifiedBody, err := json.Marshal(reqBody); err == nil {
						requestBody = modifiedBody
						c.Request.Body = io.NopCloser(bytes.NewReader(requestBody))
						c.Request.ContentLength = int64(len(requestBody))
						log.Debug("added stream_options for usage reporting",
							slog.String("provider", provider.Name),
							slog.String("model", model))
					}
				}
			}
		}

		// Route based on API type
		if provider.APIType == routing.APITypeResponses {
			// Handle Responses API (GPT-5 Pro, GPT-4.5+)
			log.Info("routing to Responses API handler",
				slog.String("model", model),
				slog.String("provider", provider.Name))

			// Extract encryption enabled header
			encryptionEnabledStr := c.GetHeader("X-Encryption-Enabled")
			if encryptionEnabledStr != "" {
				encryptionEnabled := encryptionEnabledStr == "true"
				c.Set("encryptionEnabled", &encryptionEnabled)
			}

			// Save user message to Firestore before forwarding request
			if len(requestBody) > 0 {
				saveUserMessageAsync(c, messageService, requestBody)
			}

			// Handle Responses API request (uses background polling mode)
			if err := handleResponsesAPI(c, requestBody, provider, model, log, trackingService, messageService, titleService, pollingManager, cfg); err != nil {
				log.Error("Responses API handler failed",
					slog.String("error", err.Error()),
					slog.String("model", model))
				// Error already sent to client by handler
			}
			return
		}

		// Continue with Chat Completions API (existing logic below)

		// Extract encryption enabled header
		encryptionEnabledStr := c.GetHeader("X-Encryption-Enabled")
		if encryptionEnabledStr != "" {
			encryptionEnabled := encryptionEnabledStr == "true"
			c.Set("encryptionEnabled", &encryptionEnabled)
		}
		// If header not provided, leave as nil for backward compatibility

		// Save user message to Firestore before forwarding request
		// This ensures consistent server-side timestamps and eliminates client-side storage complexity
		if len(requestBody) > 0 {
			saveUserMessageAsync(c, messageService, requestBody)
		}

		// Check if this is the first user message and trigger title generation
		if titleService != nil && len(requestBody) > 0 {
			if isFirst, firstMessage := isFirstUserMessage(requestBody); isFirst {
				chatID := c.GetHeader("X-Chat-ID")
				userID, exists := auth.GetUserID(c)

				if exists && chatID != "" {
					// Get encryption flag from context
					var encryptionEnabled *bool
					if val, exists := c.Get("encryptionEnabled"); exists {
						if boolPtr, ok := val.(*bool); ok {
							encryptionEnabled = boolPtr
						}
					}

					// Queue async title generation (non-blocking)
					// Use background context since this runs async and shouldn't be tied to request lifecycle
					go titleService.QueueTitleGeneration(context.Background(), title_generation.TitleGenerationRequest{
						UserID:            userID,
						ChatID:            chatID,
						FirstMessage:      firstMessage,
						Model:             model,
						BaseURL:           baseURL,
						Platform:          platform,
						EncryptionEnabled: encryptionEnabled,
					}, apiKey)

					log.Debug("queued title generation",
						slog.String("chat_id", chatID),
						slog.String("model", model))
				}
			}
		}

		// Parse the target URL
		target, err := url.Parse(baseURL)
		if err != nil {
			log.Error("invalid url format", slog.String("base_url", baseURL), slog.String("error", err.Error()))
			c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid URL format"})
			return
		}

		logArgs := []any{
			slog.String("target_url", target.String()+c.Request.RequestURI),
			slog.String("model", model),
			slog.Int64("request_size", max(0, c.Request.ContentLength)),
		}

		// NOTE: Request body logging removed for security/privacy reasons
		// User messages and AI responses contain sensitive data (PII, PHI, etc.)
		// Only metadata (size, model, target) is logged for debugging

		log.Info("proxy request started", logArgs...)

		// Create pending session BEFORE making upstream request (for early stop support)
		if streamManager != nil {
			chatID := c.GetHeader("X-Chat-ID")
			messageID := c.GetHeader("X-Message-ID")

			// Fall back to body IDs if headers missing
			if chatID == "" {
				if bodyID, exists := c.Get("bodyChatId"); exists {
					if idStr, ok := bodyID.(string); ok {
						chatID = idStr
					}
				}
			}
			if messageID == "" {
				if bodyID, exists := c.Get("bodyMessageId"); exists {
					if idStr, ok := bodyID.(string); ok {
						messageID = idStr
					}
				}
			}

			// Create pending session if we have valid IDs
			if chatID != "" && messageID != "" {
				streamManager.CreatePendingSession(chatID, messageID)
				log.Debug("created pending session before upstream request",
					slog.String("chat_id", chatID),
					slog.String("message_id", messageID))
			}
		}

		// Create reverse proxy for this specific target
		proxy := createReverseProxyWithPooling(target)

		// Add error handler for upstream failures
		proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
			log.Error("upstream request failed",
				slog.String("target_url", target.String()+r.RequestURI),
				slog.String("error", err.Error()),
				slog.String("method", r.Method),
				slog.Duration("time_to_error", time.Since(start)),
			)
			http.Error(w, "Bad Gateway", http.StatusBadGateway)
		}

		proxy.ModifyResponse = func(resp *http.Response) error {
			upstreamLatency := time.Since(start)
			isStreaming := strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream")

			if isStreaming {
				// Use broadcast streaming with StreamManager
				// The upstream request is now detached from client context (see request clone below)
				// This ensures streaming continues after client disconnect (saves full message to Firestore)
				return handleStreamingWithBroadcast(c, resp, log, model, upstreamLatency, trackingService, messageService, streamManager, cfg, provider)
			} else {
				return handleNonStreamingResponse(resp, log, model, upstreamLatency, c, trackingService, messageService, provider)
			}
		}

		orig := proxy.Director
		proxy.Director = func(r *http.Request) {
			orig(r)
			r.Host = target.Host

			// Inject tool definitions and capture request body
			if r.Body != nil && toolRegistry != nil {
				bodyBytes, err := io.ReadAll(r.Body)
				if err == nil {
					// Parse request body
					var reqBody map[string]interface{}
					if err := json.Unmarshal(bodyBytes, &reqBody); err == nil {
						// Extract model ID from request
						modelID := ""
						if modelField, ok := reqBody["model"].(string); ok {
							modelID = modelField
						}

						// Inject tool definitions if not already present and model supports them
						if _, hasTools := reqBody["tools"]; !hasTools {
							if tools.SupportsTools(modelID) {
								toolDefs := toolRegistry.GetDefinitions()
								if len(toolDefs) > 0 {
									reqBody["tools"] = toolDefs
									log.Debug("injected tool definitions",
										slog.Int("tool_count", len(toolDefs)),
										slog.String("model", modelID))
								}
							} else {
								log.Debug("skipped tool injection for model without tool support",
									slog.String("model", modelID))
							}
						}

						// Re-serialize with tools
						modifiedBody, err := json.Marshal(reqBody)
						if err == nil {
							bodyBytes = modifiedBody
						}
					}

					// Store original body in context for tool execution continuation
					if streamManager != nil {
						c.Set("originalRequestBody", bodyBytes)
					}

					// Restore body for upstream request
					r.Body = io.NopCloser(bytes.NewReader(bodyBytes))
					r.ContentLength = int64(len(bodyBytes))
				}
			}

			// Store provider config for tool continuation requests
			c.Set("upstreamURL", baseURL)
			c.Set("upstreamAPIKey", apiKey)

			// Set Authorization header with Bearer token for AI services
			r.Header.Set("Authorization", "Bearer "+apiKey)

			// Handle User-Agent header
			if userAgent := r.Header.Get("User-Agent"); !strings.Contains(userAgent, "OpenAI/Go") {
				r.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")
			}

			// Disable gzip compression to avoid decompression overhead for now.
			// TODO: @pottekkat check if we need to decompress and re-compress the response.
			r.Header.Set("Accept-Encoding", "identity")

			// Clean up proxy headers
			r.Header.Del("X-Forwarded-For")
			r.Header.Del("X-Real-Ip")
			r.Header.Del("X-BASE-URL") // Remove our custom header before forwarding
			r.Header.Del("X-Client-Platform")
			r.Header.Del("X-Encryption-Enabled") // Remove encryption flag before forwarding
			r.Header.Del("X-Chat-ID")            // Remove chat metadata before forwarding
			r.Header.Del("X-Message-ID")         // Remove message metadata before forwarding
		}

		// Check for early cancellation (before making upstream request)
		select {
		case <-c.Request.Context().Done():
			log.Info("client canceled request before proxy started", slog.String("target_url", target.String()))
			return
		default:
		}

		// CRITICAL: For streaming requests, bypass ReverseProxy to prevent context cancellation
		// ReverseProxy internally monitors client connection and cancels upstream even with background context
		// Solution: Start upstream request in detached background goroutine (like GPT-5 Pro polling)
		if strings.Contains(c.GetHeader("Accept"), "text/event-stream") ||
			strings.Contains(c.Request.Header.Get("Accept"), "text/event-stream") {
			// Start background streaming (handler returns immediately, streaming continues independently)
			handleStreamingInBackground(c, target, apiKey, requestBody, log, start, model, trackingService, messageService, streamManager, cfg, provider)
			return
		}

		// For non-streaming requests, use the normal reverse proxy
		proxy.ServeHTTP(c.Writer, c.Request)
	}
}

// handleStreamingInBackground starts upstream request in independent background goroutine.
// This completely decouples upstream reading from Gin handler lifecycle.
//
// FINAL SOLUTION - Graceful Context Cancellation Handling:
//
// After 10+ attempted fixes trying to isolate resp.Body from context cancellation
// (context.WithoutCancel, context.Background, HTTP/1.1-only, io.ReadAll, io.Pipe, etc.),
// we discovered the real issue was NOT in the HTTP layer but in the SESSION layer.
//
// The breakthrough:
//   - The scanner in session.go was treating context.Canceled as a FATAL error
//   - But context.Canceled during streaming is EXPECTED when clients disconnect
//   - All data read BEFORE cancellation is already buffered in session chunks
//   - We should complete SUCCESSFULLY with buffered data, not error out
//
// The fix:
//   1. Use context.Background() for isolation (prevents immediate cancellation)
//   2. Use HTTP/1.1 to avoid Go stdlib HTTP/2 bugs (ForceAttemptHTTP2: false)
//   3. Stream directly from resp.Body to session (NO buffering - better UX/memory)
//   4. In session.go: Treat context.Canceled as graceful completion (not error)
//
// Result:
//   ✅ Streaming continues after client disconnect (upstream completes)
//   ✅ All data saved to Firestore (even if client disconnects mid-stream)
//   ✅ Better UX (immediate token streaming, no delay waiting for full response)
//   ✅ Lower memory (no buffering entire response in memory)
//   ✅ Handles partial data correctly (streaming APIs send data incrementally)
//
// The handler streams to client while connected, but upstream continues even after client disconnects.
func handleStreamingInBackground(
	c *gin.Context,
	target *url.URL,
	apiKey string,
	requestBody []byte,
	log *logger.Logger,
	start time.Time,
	model string,
	trackingService *request_tracking.Service,
	messageService *messaging.Service,
	streamManager *streaming.StreamManager,
	cfg *config.Config,
	provider *routing.ProviderConfig,
) {
	// Extract session IDs early (before client might disconnect)
	chatID := c.GetHeader("X-Chat-ID")
	messageID := c.GetHeader("X-Message-ID")

	// Fall back to body IDs if headers missing
	if chatID == "" {
		if bodyID, exists := c.Get("bodyChatId"); exists {
			if idStr, ok := bodyID.(string); ok {
				chatID = idStr
			}
		}
	}
	if messageID == "" {
		if bodyID, exists := c.Get("bodyMessageId"); exists {
			if idStr, ok := bodyID.(string); ok {
				messageID = idStr
			}
		}
	}

	// Generate fallback IDs if still missing
	if chatID == "" {
		chatID = uuid.New().String()
		log.Warn("X-Chat-ID missing, generated fallback", slog.String("chat_id", chatID))
	}
	if messageID == "" {
		messageID = uuid.New().String()
		log.Warn("X-Message-ID missing, generated fallback", slog.String("message_id", messageID))
	}

	// Extract user ID and encryption settings
	userID, _ := auth.GetUserID(c)
	var encryptionEnabled *bool
	if val, exists := c.Get("encryptionEnabled"); exists {
		if boolPtr, ok := val.(*bool); ok {
			encryptionEnabled = boolPtr
		}
	}

	// CRITICAL: Copy ALL request data BEFORE starting goroutine
	// DO NOT access c.Request.* inside the goroutine - it gets cleaned up when handler returns
	requestMethod := c.Request.Method
	requestURI := c.Request.RequestURI
	requestPath := c.Request.URL.Path
	requestHeaders := make(http.Header)
	for key, values := range c.Request.Header {
		requestHeaders[key] = append([]string(nil), values...)
	}
	targetURL := target.String()

	// Create pending session BEFORE making HTTP request
	streamManager.CreatePendingSession(chatID, messageID)
	log.Debug("created pending session for background streaming",
		slog.String("chat_id", chatID),
		slog.String("message_id", messageID))

	// Start background goroutine that makes HTTP request and reads stream
	// This goroutine is COMPLETELY independent of the Gin handler lifecycle
	// DO NOT reference 'c' inside this goroutine except for already-extracted data
	go func() {
		// NUCLEAR OPTION: Use completely fresh context.Background()
		// We've tried context.WithoutCancel() but even that might have hidden ties
		// to the parent context. Use context.Background() for absolute isolation.
		// We don't need request context values since this goroutine is independent.
		ctx := context.Background()

		log.Info("background: goroutine started with fresh context.Background()",
			slog.String("chat_id", chatID),
			slog.String("message_id", messageID))

		upstreamURL := targetURL + requestURI
		req, err := http.NewRequestWithContext(ctx, requestMethod, upstreamURL, bytes.NewReader(requestBody))
		if err != nil {
			log.Error("background: failed to create upstream request",
				slog.String("error", err.Error()),
				slog.String("chat_id", chatID))
			return
		}

		// Defensive check: Verify request uses background context
		if req.Context().Err() != nil {
			log.Error("background: request context already cancelled before making request",
				slog.String("error", req.Context().Err().Error()),
				slog.String("chat_id", chatID))
			return
		}

		// Copy headers (using pre-copied headers, not c.Request.Header)
		// Skip proxy-specific headers that shouldn't be forwarded to upstream
		proxyHeaders := map[string]bool{
			"X-Base-Url":           true,
			"X-Client-Platform":    true,
			"X-Encryption-Enabled": true,
			"X-Chat-Id":            true,
			"X-Message-Id":         true,
		}
		for key, values := range requestHeaders {
			if proxyHeaders[key] {
				continue
			}
			for _, value := range values {
				req.Header.Add(key, value)
			}
		}

		req.Header.Set("Authorization", "Bearer "+apiKey)
		if req.Header.Get("User-Agent") == "" {
			req.Header.Set("User-Agent", "Mozilla/5.0")
		}
		req.Header.Set("Accept-Encoding", "identity")
		req.ContentLength = int64(len(requestBody))

		// CRITICAL: Create completely independent HTTP client
		// Do NOT use shared proxyTransport - it may have cleanup logic tied to other requests
		// This client must be 100% isolated from any Gin/ReverseProxy infrastructure
		//
		// CRITICAL: Disable HTTP/2 to prevent spurious "context canceled" errors
		// Go has a bug (https://github.com/golang/go/issues/49366) where HTTP/2
		// response bodies can return "context canceled" errors when the request
		// context is canceled, even after successful reads. This causes scanner
		// errors in our streaming code. HTTP/1.1 doesn't have this issue.
		independentClient := &http.Client{
			Transport: &http.Transport{
				MaxIdleConns:        100,
				MaxIdleConnsPerHost: 10,
				IdleConnTimeout:     90 * time.Second,
				DisableKeepAlives:   false,
				DisableCompression:  true,
				ForceAttemptHTTP2:   false, // CRITICAL: Disable HTTP/2 to prevent context canceled errors
				DialContext: (&net.Dialer{
					Timeout:   30 * time.Second,
					KeepAlive: 30 * time.Second,
				}).DialContext,
				TLSHandshakeTimeout:   30 * time.Second,
				ResponseHeaderTimeout: 120 * time.Second,
				ExpectContinueTimeout: 1 * time.Second,
			},
			Timeout: 0, // No timeout - streaming can take a long time
		}

		// Make HTTP request with independent client (no shared state)
		log.Info("background: making HTTP request to upstream",
			slog.String("url", upstreamURL),
			slog.String("chat_id", chatID))

		resp, err := independentClient.Do(req)
		if err != nil {
			log.Error("background: upstream HTTP request failed",
				slog.String("error", err.Error()),
				slog.String("chat_id", chatID),
				slog.Bool("is_context_canceled", errors.Is(err, context.Canceled)),
				slog.Bool("is_context_deadline", errors.Is(err, context.DeadlineExceeded)))

			// CRITICAL: Notify session of error so subscribers don't hang forever
			if session := streamManager.GetSession(chatID, messageID); session != nil {
				// Mark session as completed with error (this will close subscriber channels)
				session.ForceComplete(fmt.Errorf("upstream request failed: %w", err))
			}
			return
		}

		upstreamLatency := time.Since(start)

		log.Info("background: HTTP response received successfully",
			slog.String("chat_id", chatID),
			slog.Int("status", resp.StatusCode),
			slog.String("content_type", resp.Header.Get("Content-Type")),
			slog.Duration("latency", upstreamLatency))

		// Attach response body to session and start reading
		session := streamManager.GetSession(chatID, messageID)
		if session == nil {
			log.Error("background: pending session not found",
				slog.String("chat_id", chatID),
				slog.String("message_id", messageID))
			resp.Body.Close()
			return
		}

		// Set original request body for tool execution
		session.SetOriginalRequest(requestBody)
		session.SetUpstreamURL(targetURL)
		session.SetUpstreamAPIKey(apiKey)

		// REAL FIX: Buffer entire response BEFORE session starts reading
		//
		// Root cause (confirmed by logs): resp.Body.Read() returns context.Canceled
		// when client disconnects, even with context.Background() and HTTP/1.1.
		// This is a fundamental limitation of Go's HTTP library.
		//
		// Why direct streaming doesn't work:
		// - Go's HTTP transport internally monitors connection state
		// - Even with context.Background(), resp.Body can return context.Canceled
		// - The scanner fix handles this gracefully, but we only get PARTIAL data
		//
		// Solution: Read entire response into memory BEFORE client can disconnect
		// - Background goroutine reads full response with io.ReadAll()
		// - By the time client subscribes and can disconnect, data is in memory
		// - Memory buffer is immune to any context cancellation
		// - Session reads from bytes.NewReader (pure memory, no network)
		//
		// Trade-off: Uses memory (~100KB typical), but guarantees complete responses
		log.Info("background: buffering entire response to prevent cancellation",
			slog.String("chat_id", chatID))

		// Read entire response body into memory buffer
		bodyBytes, err := io.ReadAll(resp.Body)
		resp.Body.Close()

		if err != nil {
			// Even io.ReadAll can get context.Canceled, but less likely since it reads quickly
			if errors.Is(err, context.Canceled) {
				log.Warn("background: io.ReadAll interrupted by cancellation, using partial data",
					slog.String("chat_id", chatID),
					slog.Int("bytes_read", len(bodyBytes)))
				// Continue with partial data - better than nothing
			} else {
				log.Error("background: failed to read response body",
					slog.String("error", err.Error()),
					slog.String("chat_id", chatID))
				session.ForceComplete(fmt.Errorf("failed to read response body: %w", err))
				return
			}
		}

		log.Info("background: response fully buffered in memory",
			slog.String("chat_id", chatID),
			slog.Int("bytes", len(bodyBytes)))

		// Create memory reader - 100% immune to context cancellation
		memoryReader := io.NopCloser(bytes.NewReader(bodyBytes))

		// Now session reads from memory buffer, not network
		session.SetUpstreamBodyAndStart(memoryReader)

		log.Info("background: upstream reading started, goroutine will continue until completion",
			slog.String("chat_id", chatID),
			slog.String("message_id", messageID))

		// Wait for session to complete (this goroutine stays alive until stream finishes)
		session.WaitForCompletion()

		// Check if session had errors
		if err := session.GetError(); err != nil {
			log.Error("background: session completed with error",
				slog.String("error", err.Error()),
				slog.String("chat_id", chatID),
				slog.String("message_id", messageID))
		}

		// Save to Firestore after completion
		if userID != "" && messageService != nil {
			err := streamManager.SaveCompletedSession(ctx, session, userID, encryptionEnabled, model)
			if err != nil {
				log.Error("background: failed to save completed session",
					slog.String("error", err.Error()),
					slog.String("chat_id", chatID))
			}
		}

		// Log request to database
		sessionUsage := session.GetTokenUsage()
		var tokenUsage *Usage
		if sessionUsage != nil {
			tokenUsage = &Usage{
				PromptTokens:     sessionUsage.PromptTokens,
				CompletionTokens: sessionUsage.CompletionTokens,
				TotalTokens:      sessionUsage.TotalTokens,
			}
		}

		// Create minimal request info for logging
		info := request_tracking.RequestInfo{
			UserID:   userID,
			Endpoint: requestPath,
			Model:    model,
			Provider: provider.Name,
		}

		if tokenUsage != nil && provider.TokenMultiplier > 0 && trackingService != nil {
			planTokens := int(float64(tokenUsage.TotalTokens) * provider.TokenMultiplier)
			tokenData := &request_tracking.TokenUsageWithMultiplier{
				PromptTokens:     tokenUsage.PromptTokens,
				CompletionTokens: tokenUsage.CompletionTokens,
				TotalTokens:      tokenUsage.TotalTokens,
				Multiplier:       provider.TokenMultiplier,
				PlanTokens:       planTokens,
			}
			trackingService.LogRequestWithPlanTokensAsync(ctx, info, tokenData) //nolint:errcheck
		}

		log.Info("background: streaming completed and saved",
			slog.String("chat_id", chatID),
			slog.String("message_id", messageID))
	}()

	// Meanwhile, in the foreground: subscribe to session and stream to client
	// CRITICAL: Do NOT wait for background goroutine - subscribe immediately
	// This decouples client connection from upstream request entirely
	session := streamManager.GetSession(chatID, messageID)
	if session == nil {
		log.Error("pending session not found for client streaming",
			slog.String("chat_id", chatID))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create session"})
		return
	}

	// Set response headers for SSE immediately
	c.Writer.Header().Set("Content-Type", "text/event-stream")
	c.Writer.Header().Set("Cache-Control", "no-cache")
	c.Writer.Header().Set("Connection", "keep-alive")
	c.Writer.Header().Set("X-Accel-Buffering", "no")
	c.Writer.WriteHeader(http.StatusOK)

	// Subscribe immediately (even if session hasn't started yet)
	// Subscriber will receive chunks as they arrive via channel
	// Use client's request context so subscriber cleanup happens on disconnect
	subscriberID := uuid.New().String()
	subscriber, err := session.Subscribe(c.Request.Context(), subscriberID, streaming.SubscriberOptions{
		ReplayFromStart: false,
		BufferSize:      100,
	})
	if err != nil {
		log.Error("failed to subscribe to session",
			slog.String("error", err.Error()),
			slog.String("chat_id", chatID))
		return
	}

	streamManager.RecordSubscription()

	log.Info("client subscribed, will receive chunks as they arrive",
		slog.String("chat_id", chatID),
		slog.String("subscriber_id", subscriberID))

	// Stream to client (blocks until client disconnects or stream completes)
	// When this returns, handler ends, but background goroutine continues
	streamToClient(c, subscriber, session, log)

	// Handler returns here (client may have disconnected)
	// But background goroutine continues reading and saving!
}

// handleStreamingResponse extracts token usage from streaming responses.
func handleStreamingResponse(resp *http.Response, log *logger.Logger, model string, upstreamLatency time.Duration, c *gin.Context, trackingService *request_tracking.Service, messageService *messaging.Service, provider *routing.ProviderConfig) error {
	pr, pw := io.Pipe()
	originalBody := resp.Body
	resp.Body = pr

	go func() {
		defer pw.Close()           //nolint:errcheck
		defer originalBody.Close() //nolint:errcheck

		defer func() {
			if r := recover(); r != nil {
				log.Error("panic in streaming response handler",
					slog.Any("panic", r),
					slog.String("target_url", resp.Request.URL.String()),
					slog.String("provider", request_tracking.GetProviderFromBaseURL(c.GetHeader("X-BASE-URL"))),
				)
			}
		}()

		var reader io.Reader = originalBody

		scanner := bufio.NewScanner(reader)
		scanner.Buffer(make([]byte, 64*1024), 1024*1024) // 64KB initial, 1MB max.
		var tokenUsage *Usage
		var fullContent strings.Builder // Accumulate full response content

		// CRITICAL FIX: Use defer to ALWAYS log, even if client disconnects early
		// Without this, streaming requests were not logged when client disconnected before [DONE]
		defer func() {
			// Debug: Log why we might have NULL tokens
			if tokenUsage == nil {
				log.Warn("no token usage captured from streaming response",
					slog.String("model", model),
					slog.String("provider", provider.Name),
					slog.Int("content_length", fullContent.Len()),
					slog.String("reason", "client_disconnect_or_missing_usage_chunk"))
			}

			logProxyResponse(log, resp, true, upstreamLatency, model, tokenUsage, nil, c.Request.Context())

			// Log with multiplier if provider is available
			if provider != nil {
				logRequestToDatabaseWithProvider(c, trackingService, model, tokenUsage, provider.Name, provider.TokenMultiplier)
			} else {
				logRequestToDatabase(c, trackingService, model, tokenUsage)
			}

			// Save message to Firestore asynchronously
			isError := resp.StatusCode >= 400
			saveMessageAsync(c, messageService, fullContent.String(), isError)
		}()

		clientCtx := c.Request.Context()
		clientDisconnected := false

		for scanner.Scan() {
			line := scanner.Text()

			// Check if client disconnected
			select {
			case <-clientCtx.Done():
				if !clientDisconnected {
					log.Debug("client disconnected, continuing to read for token usage")
					clientDisconnected = true
				}
			default:
			}

			// Only pipe to client if still connected
			if !clientDisconnected {
				if _, err := pw.Write(append([]byte(line), '\n')); err != nil {
					log.Debug("failed to write to pipe (client likely disconnected)", slog.String("error", err.Error()))
					clientDisconnected = true
				}
			}

			// Extract and accumulate content for message storage
			if content := extractContentFromSSELine(line); content != "" {
				fullContent.WriteString(content)
			}

			// Extract the token usage from second to last chunk which contains a usage field.
			// See: https://openrouter.ai/docs/use-cases/usage-accounting#streaming-with-usage-information
			if usage := extractTokenUsageFromSSELine(line); usage != nil {
				tokenUsage = usage
			}
		}

		if err := scanner.Err(); err != nil {
			log.Error("scanner error while processing SSE stream", slog.String("error", err.Error()))
		}

		// Note: Logging now happens in defer above, so it runs whether we reach here or return early
	}()

	// Remove Content-Length for chunked encoding.
	resp.Header.Del("Content-Length")
	return nil
}

// handleNonStreamingResponse extracts token usage from non-streaming responses.
func handleNonStreamingResponse(resp *http.Response, log *logger.Logger, model string, upstreamLatency time.Duration, c *gin.Context, trackingService *request_tracking.Service, messageService *messaging.Service, provider *routing.ProviderConfig) error {
	var responseBody []byte
	if resp.Body != nil {
		responseBody, _ = io.ReadAll(resp.Body)
		resp.Body = io.NopCloser(bytes.NewReader(responseBody))
	}

	var tokenUsage *Usage
	var content string
	if len(responseBody) > 0 {
		tokenUsage = extractTokenUsage(responseBody)
		content = extractContentFromResponse(responseBody)

		if tokenUsage == nil {
			log.Debug("No token usage found in response",
				slog.Bool("is_streaming", false),
				slog.Int("response_size", len(responseBody)),
				slog.String("content_type", resp.Header.Get("Content-Type")),
			)
		}
	}

	logProxyResponse(log, resp, false, upstreamLatency, model, tokenUsage, responseBody, c.Request.Context())

	// Log with multiplier if provider is available
	if provider != nil {
		logRequestToDatabaseWithProvider(c, trackingService, model, tokenUsage, provider.Name, provider.TokenMultiplier)
	} else {
		logRequestToDatabase(c, trackingService, model, tokenUsage)
	}

	// Save message to Firestore asynchronously
	isError := resp.StatusCode >= 400
	saveMessageAsync(c, messageService, content, isError)

	return nil
}

// logProxyResponse logs the final proxy response with consolidated token usage data.
func logProxyResponse(log *logger.Logger, resp *http.Response, isStreaming bool, upstreamLatency time.Duration, model string, tokenUsage *Usage, responseBody []byte, ctx context.Context) {
	responseLogArgs := []any{
		slog.Int("status_code", resp.StatusCode),
		slog.String("content_type", resp.Header.Get("Content-Type")),
		slog.Bool("is_streaming", isStreaming),
		slog.Duration("upstream_latency", upstreamLatency),
		slog.String("response_id", resp.Header.Get("X-Request-ID")),
	}

	if tokenUsage != nil {
		responseLogArgs = append(responseLogArgs,
			slog.Int("prompt_tokens", tokenUsage.PromptTokens),
			slog.Int("completion_tokens", tokenUsage.CompletionTokens),
			slog.Int("total_tokens", tokenUsage.TotalTokens),
			slog.String("model", model),
		)
	}

	// NOTE: Response body logging removed for security/privacy reasons
	// AI responses contain sensitive user data (PII, PHI, financial data, etc.)
	// Only metadata (status, size, duration, model) is logged for debugging

	log.Info("proxy response received", responseLogArgs...)
}

// logRequestToDatabase logs a request to the database with token usage data.
func logRequestToDatabase(c *gin.Context, trackingService *request_tracking.Service, model string, tokenUsage *Usage) {
	logRequestToDatabaseWithProvider(c, trackingService, model, tokenUsage, "", 1.0)
}

func logRequestToDatabaseWithProvider(c *gin.Context, trackingService *request_tracking.Service, model string, tokenUsage *Usage, providerName string, multiplier float64) {
	userID, exists := auth.GetUserID(c)
	if !exists {
		return
	}

	var provider string
	if providerName != "" {
		provider = providerName
	} else {
		baseURL := c.GetHeader("X-BASE-URL")
		provider = request_tracking.GetProviderFromBaseURL(baseURL)
	}
	endpoint := c.Request.URL.Path

	info := request_tracking.RequestInfo{
		UserID:   userID,
		Endpoint: endpoint,
		Model:    model,
		Provider: provider,
	}

	// Always log the request, even without token usage
	// This ensures errors, audio, images, and other endpoints are tracked
	if tokenUsage != nil && multiplier > 0 {
		// Best case: Log with plan tokens
		planTokens := int(float64(tokenUsage.TotalTokens) * multiplier)
		tokenData := &request_tracking.TokenUsageWithMultiplier{
			PromptTokens:     tokenUsage.PromptTokens,
			CompletionTokens: tokenUsage.CompletionTokens,
			TotalTokens:      tokenUsage.TotalTokens,
			Multiplier:       multiplier,
			PlanTokens:       planTokens,
		}
		if err := trackingService.LogRequestWithPlanTokensAsync(c.Request.Context(), info, tokenData); err != nil {
			if loggerValue := c.Value("logger"); loggerValue != nil {
				if log, ok := loggerValue.(*logger.Logger); ok {
					log.Error("failed to log request with plan tokens to database",
						slog.String("user_id", userID),
						slog.String("endpoint", endpoint),
						slog.Float64("multiplier", multiplier),
						slog.Int("plan_tokens", planTokens),
						slog.String("error", err.Error()))
				}
			}
		}
	} else if tokenUsage != nil {
		// Fallback: Log with tokens but no multiplier
		tokenData := &request_tracking.TokenUsage{
			PromptTokens:     tokenUsage.PromptTokens,
			CompletionTokens: tokenUsage.CompletionTokens,
			TotalTokens:      tokenUsage.TotalTokens,
		}
		if err := trackingService.LogRequestWithTokensAsync(c.Request.Context(), info, tokenData); err != nil {
			if loggerValue := c.Value("logger"); loggerValue != nil {
				if log, ok := loggerValue.(*logger.Logger); ok {
					log.Error("failed to log request to database",
						slog.String("user_id", userID),
						slog.String("endpoint", endpoint),
						slog.String("error", err.Error()))
				}
			}
		}
	} else {
		// Critical fix: Log request even without token usage
		// This captures errors, audio, images, and requests where provider didn't return usage
		if err := trackingService.LogRequestAsync(c.Request.Context(), info); err != nil {
			if loggerValue := c.Value("logger"); loggerValue != nil {
				if log, ok := loggerValue.(*logger.Logger); ok {
					log.Error("failed to log request to database (no token data)",
						slog.String("user_id", userID),
						slog.String("endpoint", endpoint),
						slog.String("error", err.Error()))
				}
			}
		}
	}
}
