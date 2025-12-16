package auth

import (
	"bytes"
	"io"
	"log"
	"net/http"
	"strings"
)

// LoggingTransport wraps http.RoundTripper to log requests and responses
type LoggingTransport struct {
	Transport http.RoundTripper
}

func (t *LoggingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// Only log FCM requests
	if strings.Contains(req.URL.String(), "fcm.googleapis.com") {
		log.Printf("========== FCM SDK HTTP REQUEST ==========")
		log.Printf("Method: %s", req.Method)
		log.Printf("URL: %s", req.URL.String())
		log.Printf("Headers:")
		for name, values := range req.Header {
			for _, value := range values {
				// Log Authorization header but truncate the token
				if name == "Authorization" && len(value) > 50 {
					log.Printf("  %s: %s...%s", name, value[:30], value[len(value)-20:])
				} else {
					log.Printf("  %s: %s", name, value)
				}
			}
		}

		if req.Body != nil {
			bodyBytes, err := io.ReadAll(req.Body)
			if err == nil {
				log.Printf("Body: %s", string(bodyBytes))
				// Restore body for actual request
				req.Body = io.NopCloser(bytes.NewBuffer(bodyBytes))
			}
		}
	}

	// Execute the request
	resp, err := t.Transport.RoundTrip(req)

	// Log response for FCM requests
	if strings.Contains(req.URL.String(), "fcm.googleapis.com") {
		if err != nil {
			log.Printf("Response: ERROR - %v", err)
		} else {
			log.Printf("Response Status: %d %s", resp.StatusCode, resp.Status)
			if resp.Body != nil {
				bodyBytes, readErr := io.ReadAll(resp.Body)
				if readErr == nil {
					log.Printf("Response Body: %s", string(bodyBytes))
					// Restore body for caller
					resp.Body = io.NopCloser(bytes.NewBuffer(bodyBytes))
				}
			}
		}
		log.Printf("==========================================")
	}

	return resp, err
}

// NewLoggingTransport creates a new logging transport
func NewLoggingTransport() *LoggingTransport {
	return &LoggingTransport{
		Transport: http.DefaultTransport,
	}
}
