package auth

import (
	"crypto/subtle"
	"strings"

	"github.com/eternisai/enchanted-proxy/internal/errors"
	"github.com/eternisai/enchanted-proxy/internal/logger"
	"github.com/gin-gonic/gin"
)

// Define a custom type for context keys to avoid collisions.
type contextKey string

const (
	UserIDKey contextKey = "user_id"
)

type FirebaseAuthMiddleware struct {
	validator TokenValidator
}

func NewFirebaseAuthMiddleware(validator TokenValidator) (*FirebaseAuthMiddleware, error) {
	return &FirebaseAuthMiddleware{
		validator: validator,
	}, nil
}

// RequireAuth is a middleware that validates Firebase tokens and attaches user UUID to context.
func (f *FirebaseAuthMiddleware) RequireAuth() gin.HandlerFunc {
	return func(c *gin.Context) {
		// Extract Authorization header

		authHeader := c.GetHeader("Authorization")

		// Fallback for WebSocket connections: accept token from query parameter
		// Browser WebSocket API doesn't support custom headers during upgrade
		if authHeader == "" && c.Request.Header.Get("Upgrade") == "websocket" {
			token := c.Query("token")
			if token != "" {
				authHeader = "Bearer " + token
			}
		}

		if authHeader == "" {
			errors.AbortWithUnauthorized(c, "Authorization header is required", nil)
			return
		}

		// Check if it's a Bearer token
		if !strings.HasPrefix(authHeader, "Bearer ") {
			errors.AbortWithUnauthorized(c, "Authorization header must be a Bearer token", nil)
			return
		}

		// Extract the token
		token := strings.TrimPrefix(authHeader, "Bearer ")
		if token == "" {
			errors.AbortWithUnauthorized(c, "Bearer token is empty", nil)
			return
		}

		userID, err := f.validator.ExtractUserID(token)
		if err != nil {
			errors.AbortWithUnauthorized(c, "Invalid or expired token", nil)
			return
		}

		ctx := logger.WithUserID(c.Request.Context(), userID)
		c.Request = c.Request.WithContext(ctx)
		c.Set(string(UserIDKey), userID)

		c.Next()
	}
}

func GetUserID(c *gin.Context) (string, bool) {
	userID, exists := c.Get(string(UserIDKey))
	if !exists {
		return "", false
	}

	id, ok := userID.(string)
	return id, ok
}

// APIKeyMiddleware validates requests using a static API key.
type APIKeyMiddleware struct {
	apiKey string
}

// NewAPIKeyMiddleware creates a new API key middleware with the provided key.
func NewAPIKeyMiddleware(apiKey string) *APIKeyMiddleware {
	return &APIKeyMiddleware{
		apiKey: apiKey,
	}
}

// RequireAPIKey is a middleware that validates Bearer token against the configured API key.
func (a *APIKeyMiddleware) RequireAPIKey() gin.HandlerFunc {
	return func(c *gin.Context) {
		authHeader := c.GetHeader("Authorization")

		if authHeader == "" {
			errors.AbortWithUnauthorized(c, "Authorization header is required", nil)
			return
		}

		if !strings.HasPrefix(authHeader, "Bearer ") {
			errors.AbortWithUnauthorized(c, "Authorization header must be a Bearer token", nil)
			return
		}

		token := strings.TrimPrefix(authHeader, "Bearer ")
		if token == "" {
			errors.AbortWithUnauthorized(c, "Bearer token is empty", nil)
			return
		}

		// Use constant-time comparison to prevent timing attacks
		if subtle.ConstantTimeCompare([]byte(token), []byte(a.apiKey)) != 1 {
			errors.AbortWithUnauthorized(c, "Invalid API key", nil)
			return
		}

		c.Next()
	}
}
