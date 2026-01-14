package auth

import (
	"net/http"
	"strings"

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
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Authorization header is required"})
			c.Abort()
			return
		}

		// Check if it's a Bearer token
		if !strings.HasPrefix(authHeader, "Bearer ") {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Authorization header must be a Bearer token"})
			c.Abort()
			return
		}

		// Extract the token
		token := strings.TrimPrefix(authHeader, "Bearer ")
		if token == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Bearer token is empty"})
			c.Abort()
			return
		}

		userID, err := f.validator.ExtractUserID(token)
		if err != nil {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Invalid or expired token"})
			c.Abort()
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
