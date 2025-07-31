package proxy

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"github.com/eternisai/enchanted-proxy/internal/config"
	"github.com/eternisai/enchanted-proxy/internal/logger"
	"github.com/gin-gonic/gin"
)

func ProxyHandler(logger *logger.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		log := logger.WithContext(c.Request.Context()).WithComponent("proxy")

		var requestBody []byte
		var model string
		if c.Request.Body != nil {
			requestBody, err := io.ReadAll(c.Request.Body)
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

			log.Info("proxy response received",
				slog.Int("status_code", resp.StatusCode),
				slog.String("content_type", resp.Header.Get("Content-Type")),
				slog.Bool("is_streaming", isStreaming),
				slog.Duration("upstream_latency", upstreamLatency),
				slog.String("response_id", resp.Header.Get("X-Request-ID")),
			)
			return nil
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
