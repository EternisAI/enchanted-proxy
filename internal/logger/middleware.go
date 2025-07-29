package logger

import (
	"log/slog"
	"time"

	"github.com/gin-gonic/gin"
)

// RequestLoggingMiddleware logs all incoming requests.
// It generates a requestID, adds it to the context, and then logs request details.
func RequestLoggingMiddleware(logger *Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()

		// Reuse the request ID from the request headers if present.
		requestID := c.Request.Header.Get("x-request-id")
		if requestID == "" {
			// Generate request ID and add to context.
			requestID = GenerateRequestID()
		}
		ctx := WithRequestID(c.Request.Context(), requestID)
		ctx = WithOperation(ctx, "http_request")
		c.Request = c.Request.WithContext(ctx)

		// Create contextual logger.
		log := logger.WithContext(ctx).WithComponent("http")

		// Log request start.
		log.Info("request started",
			slog.String("method", c.Request.Method),
			slog.String("path", c.Request.URL.Path),
			slog.String("remote_addr", c.ClientIP()),
			slog.String("user_agent", c.Request.UserAgent()),
		)

		c.Next()

		// Log request completion.
		duration := time.Since(start)
		log.Info("request completed",
			slog.Int("status", c.Writer.Status()),
			slog.Duration("duration", duration),
			slog.Int("response_size", c.Writer.Size()),
		)
	}
}
