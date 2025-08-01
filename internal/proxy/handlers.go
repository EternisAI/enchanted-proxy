package proxy

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"github.com/eternisai/enchanted-proxy/internal/auth"
	"github.com/eternisai/enchanted-proxy/internal/config"
	"github.com/eternisai/enchanted-proxy/internal/logger"
	"github.com/eternisai/enchanted-proxy/internal/request_tracking"
	"github.com/gin-gonic/gin"
)

func ProxyHandler(logger *logger.Logger, trackingService *request_tracking.Service) gin.HandlerFunc {
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

		// Check if base URL is in our allowed dictionary
		apiKey := getAPIKey(baseURL, config.AppConfig)
		if apiKey == "" {
			log.Warn("unauthorized base url", slog.String("base_url", baseURL))
			c.JSON(http.StatusForbidden, gin.H{"error": "Unauthorized base URL"})
			return
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
		proxy := httputil.NewSingleHostReverseProxy(target)

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
				return handleStreamingResponse(resp, log, model, upstreamLatency, c, trackingService)
			} else {
				return handleNonStreamingResponse(resp, log, model, upstreamLatency, c, trackingService)
			}
		}

		orig := proxy.Director
		proxy.Director = func(r *http.Request) {
			orig(r)
			r.Host = target.Host

			// Set Authorization header with Bearer token
			r.Header.Set("Authorization", "Bearer "+apiKey)

			// Handle User-Agent header
			if userAgent := r.Header.Get("User-Agent"); !strings.Contains(userAgent, "OpenAI/Go") {
				r.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")
			}

			// Clean up proxy headers
			r.Header.Del("X-Forwarded-For")
			r.Header.Del("X-Real-Ip")
			r.Header.Del("X-BASE-URL") // Remove our custom header before forwarding
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
func handleStreamingResponse(resp *http.Response, log *logger.Logger, model string, upstreamLatency time.Duration, c *gin.Context, trackingService *request_tracking.Service) error {
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

		// Decompress gzipped responses if needed.
		if strings.Contains(resp.Header.Get("Content-Encoding"), "gzip") {
			gzReader, err := gzip.NewReader(originalBody)
			if err != nil {
				log.Error("failed to create gzip reader for streaming response", slog.String("error", err.Error()))
				return
			}
			defer gzReader.Close() //nolint:errcheck
			reader = gzReader
			// Remove Content-Encoding header since we're decompressing.
			// TODO: @pottekkat recompress the response body before sending it to the client.
			resp.Header.Del("Content-Encoding")
		}

		scanner := bufio.NewScanner(reader)
		var responseBodyBuilder strings.Builder
		var tokenUsage *Usage

		for scanner.Scan() {
			line := scanner.Text()

			// Pipe the line to the client immediately.
			if _, err := pw.Write(append([]byte(line), '\n')); err != nil {
				log.Error("failed to write to pipe", slog.String("error", err.Error()))
				return
			}

			responseBodyBuilder.WriteString(line)
			responseBodyBuilder.WriteByte('\n')

			// Extract the token usage from second to last chunk which contains a usage field.
			// See: https://openrouter.ai/docs/use-cases/usage-accounting#streaming-with-usage-information
			if usage := extractTokenUsageFromSSELine(line); usage != nil {
				tokenUsage = usage
			}
		}

		if err := scanner.Err(); err != nil {
			log.Error("scanner error while processing SSE stream", slog.String("error", err.Error()))
		}

		responseBody := responseBodyBuilder.String()
		logProxyResponse(log, resp, true, upstreamLatency, model, tokenUsage, []byte(responseBody), c.Request.Context())

		logRequestToDatabase(c, trackingService, model, tokenUsage)
	}()

	// Remove Content-Length for chunked encoding.
	resp.Header.Del("Content-Length")
	return nil
}

// handleNonStreamingResponse extracts token usage from non-streaming responses.
func handleNonStreamingResponse(resp *http.Response, log *logger.Logger, model string, upstreamLatency time.Duration, c *gin.Context, trackingService *request_tracking.Service) error {
	var responseBody []byte
	if resp.Body != nil {
		responseBody, _ = io.ReadAll(resp.Body)
		resp.Body = io.NopCloser(bytes.NewReader(responseBody))

		if strings.Contains(resp.Header.Get("Content-Encoding"), "gzip") && len(responseBody) > 0 {
			if gzReader, err := gzip.NewReader(bytes.NewReader(responseBody)); err == nil {
				if decompressed, err := io.ReadAll(gzReader); err == nil {
					responseBody = decompressed
				}
				defer gzReader.Close() //nolint:errcheck
			}
		}
	}

	var tokenUsage *Usage
	if len(responseBody) > 0 {
		tokenUsage = extractTokenUsage(responseBody)

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
