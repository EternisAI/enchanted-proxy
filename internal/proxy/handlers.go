package proxy

import (
	"bufio"
	"bytes"
	"context"
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
	"github.com/eternisai/enchanted-proxy/internal/config"
	"github.com/eternisai/enchanted-proxy/internal/logger"
	"github.com/eternisai/enchanted-proxy/internal/messaging"
	"github.com/eternisai/enchanted-proxy/internal/request_tracking"
	"github.com/eternisai/enchanted-proxy/internal/title_generation"
	"github.com/gin-gonic/gin"
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

func ProxyHandler(logger *logger.Logger, trackingService *request_tracking.Service, messageService *messaging.Service, titleService *title_generation.Service) gin.HandlerFunc {
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

			model = extractModelFromRequestBody(c.Request.URL.Path, requestBody)
		}

		// Extract X-BASE-URL from header
		baseURL := c.GetHeader("X-BASE-URL")
		if baseURL == "" {
			log.Warn("missing base url header")
			c.JSON(http.StatusBadRequest, gin.H{"error": "X-BASE-URL header is required"})
			return
		}

		platform := c.GetHeader("X-Client-Platform")
		if platform == "" {
			log.Warn("missing client platform header, defaulting to mobile")
			platform = "mobile"
		}

		// Extract encryption enabled header
		encryptionEnabledStr := c.GetHeader("X-Encryption-Enabled")
		if encryptionEnabledStr != "" {
			encryptionEnabled := encryptionEnabledStr == "true"
			c.Set("encryptionEnabled", &encryptionEnabled)
		}
		// If header not provided, leave as nil for backward compatibility

		// Check if base URL is in our allowed dictionary
		apiKey := GetAPIKey(baseURL, platform, config.AppConfig)
		if apiKey == "" {
			log.Warn("unauthorized base url", slog.String("base_url", baseURL))
			c.JSON(http.StatusForbidden, gin.H{"error": "Unauthorized base URL"})
			return
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

		if log.Enabled(c.Request.Context(), slog.LevelDebug) {
			logArgs = append(logArgs, slog.String("request_body", logRequestBody(requestBody, 300)))
		}

		log.Info("proxy request started", logArgs...)

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
				return handleStreamingResponse(resp, log, model, upstreamLatency, c, trackingService, messageService)
			} else {
				return handleNonStreamingResponse(resp, log, model, upstreamLatency, c, trackingService, messageService)
			}
		}

		orig := proxy.Director
		proxy.Director = func(r *http.Request) {
			orig(r)
			r.Host = target.Host

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

		// Some canceled requests by clients could cause panic.
		// We handle that gracefully.
		// See: https://github.com/gin-gonic/gin/issues/2279
		select {
		case <-c.Request.Context().Done():
			log.Info("client canceled request", slog.String("target_url", target.String()))
			return
		default:
			proxy.ServeHTTP(c.Writer, c.Request)
		}
	}
}

// handleStreamingResponse extracts token usage from streaming responses.
func handleStreamingResponse(resp *http.Response, log *logger.Logger, model string, upstreamLatency time.Duration, c *gin.Context, trackingService *request_tracking.Service, messageService *messaging.Service) error {
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
		var firstChunk string
		var fullContent strings.Builder // Accumulate full response content

		ctx := c.Request.Context()
		for scanner.Scan() {
			select {
			case <-ctx.Done():
				log.Debug("client disconnected, stopping stream processing")
				return
			default:
			}

			line := scanner.Text()

			// Pipe the line to the client immediately.
			if _, err := pw.Write(append([]byte(line), '\n')); err != nil {
				log.Error("failed to write to pipe", slog.String("error", err.Error()))
				return
			}

			if firstChunk == "" && log.Enabled(ctx, slog.LevelDebug) && strings.HasPrefix(line, "data: ") && !strings.Contains(line, "[DONE]") {
				firstChunk = line
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

		logProxyResponse(log, resp, true, upstreamLatency, model, tokenUsage, []byte(firstChunk), c.Request.Context())

		logRequestToDatabase(c, trackingService, model, tokenUsage)

		// Save message to Firestore asynchronously
		isError := resp.StatusCode >= 400
		saveMessageAsync(c, messageService, fullContent.String(), isError)
	}()

	// Remove Content-Length for chunked encoding.
	resp.Header.Del("Content-Length")
	return nil
}

// handleNonStreamingResponse extracts token usage from non-streaming responses.
func handleNonStreamingResponse(resp *http.Response, log *logger.Logger, model string, upstreamLatency time.Duration, c *gin.Context, trackingService *request_tracking.Service, messageService *messaging.Service) error {
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

	logRequestToDatabase(c, trackingService, model, tokenUsage)

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

	// Add response body to debug logs.
	if log.Enabled(ctx, slog.LevelDebug) && len(responseBody) > 0 {
		responseLogArgs = append(responseLogArgs, slog.String("response_body", logRequestBody(responseBody, 500)))
	}

	log.Info("proxy response received", responseLogArgs...)
}

// logRequestToDatabase logs a request to the database with token usage data.
func logRequestToDatabase(c *gin.Context, trackingService *request_tracking.Service, model string, tokenUsage *Usage) {
	userID, exists := auth.GetUserUUID(c)
	if !exists {
		return
	}

	baseURL := c.GetHeader("X-BASE-URL")
	provider := request_tracking.GetProviderFromBaseURL(baseURL)
	endpoint := c.Request.URL.Path

	info := request_tracking.RequestInfo{
		UserID:   userID,
		Endpoint: endpoint,
		Model:    model,
		Provider: provider,
	}

	var tokenData *request_tracking.TokenUsage
	if tokenUsage != nil {
		tokenData = &request_tracking.TokenUsage{
			PromptTokens:     tokenUsage.PromptTokens,
			CompletionTokens: tokenUsage.CompletionTokens,
			TotalTokens:      tokenUsage.TotalTokens,
		}
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
}
