package auth

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/eternisai/enchanted-proxy/internal/logger"
)

// LoggingTransport wraps http.RoundTripper to log requests and responses
type LoggingTransport struct {
	Transport http.RoundTripper
	Logger    *logger.Logger
}

func (t *LoggingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// Only log FCM requests
	if strings.Contains(req.URL.String(), "fcm.googleapis.com") {
		// Extract OAuth token from Authorization header
		authHeader := req.Header.Get("Authorization")
		var tokenPrefix, tokenSuffix string
		if strings.HasPrefix(authHeader, "Bearer ") {
			token := strings.TrimPrefix(authHeader, "Bearer ")
			if len(token) > 50 {
				tokenPrefix = token[:30]
				tokenSuffix = token[len(token)-20:]
			} else {
				tokenPrefix = token
				tokenSuffix = ""
			}
		}

		// Read request body
		var requestBody string
		if req.Body != nil {
			bodyBytes, err := io.ReadAll(req.Body)
			if err == nil {
				requestBody = string(bodyBytes)
				// Restore body for actual request
				req.Body = io.NopCloser(bytes.NewBuffer(bodyBytes))
			}
		}

		t.Logger.Info("FCM SDK HTTP request",
			slog.String("method", req.Method),
			slog.String("url", req.URL.String()),
			slog.String("oauth_token_prefix", tokenPrefix),
			slog.String("oauth_token_suffix", tokenSuffix),
			slog.String("content_type", req.Header.Get("Content-Type")),
			slog.String("request_body", requestBody))
	}

	// Execute the request
	resp, err := t.Transport.RoundTrip(req)

	// Log response for FCM requests
	if strings.Contains(req.URL.String(), "fcm.googleapis.com") {
		if err != nil {
			t.Logger.Error("FCM SDK HTTP request failed",
				slog.String("error", err.Error()))
		} else {
			// Read response body
			var responseBody string
			if resp.Body != nil {
				bodyBytes, readErr := io.ReadAll(resp.Body)
				if readErr == nil {
					responseBody = string(bodyBytes)
					// Restore body for caller
					resp.Body = io.NopCloser(bytes.NewBuffer(bodyBytes))
				}
			}

			if resp.StatusCode >= 400 {
				t.Logger.Error("FCM SDK HTTP response error",
					slog.Int("status_code", resp.StatusCode),
					slog.String("status", resp.Status),
					slog.String("response_body", responseBody))
			} else {
				t.Logger.Info("FCM SDK HTTP response",
					slog.Int("status_code", resp.StatusCode),
					slog.String("response_body", responseBody))
			}
		}
	}

	return resp, err
}

// NewLoggingTransport creates a new logging transport
func NewLoggingTransport(log *logger.Logger) *LoggingTransport {
	return &LoggingTransport{
		Transport: http.DefaultTransport,
		Logger:    log,
	}
}
