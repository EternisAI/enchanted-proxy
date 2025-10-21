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
	// UserUUIDKey is the context key for user UUID.
	UserUUIDKey contextKey = "user_uuid"
	// UserIDKey is the context key for Firebase UID (for Firestore paths).
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

		// Validate the token with Firebase
		userUUID, err := f.validator.ValidateToken(token)
		if err != nil {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Invalid or expired token"})
			c.Abort()
			return
		}

		// Extract Firebase UID for Firestore paths
		userID, err := f.validator.ExtractUserID(token)
		if err != nil {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Invalid or expired token"})
			c.Abort()
			return
		}

		// Attach user UUID to both Gin context and request context
		ctx := logger.WithUserID(c.Request.Context(), userUUID)
		c.Request = c.Request.WithContext(ctx)
		c.Set(string(UserUUIDKey), userUUID)
		c.Set(string(UserIDKey), userID)
		c.Next()
	}
}

// GetUserUUID extracts the user UUID from the Gin context.
func GetUserUUID(c *gin.Context) (string, bool) {
	userUUID, exists := c.Get(string(UserUUIDKey))
	if !exists {
		return "", false
	}

	uuid, ok := userUUID.(string)
	return uuid, ok
}

// GetUserID extracts the Firebase UID from the Gin context.
// This should be used for Firestore paths instead of GetUserUUID.
func GetUserID(c *gin.Context) (string, bool) {
	userID, exists := c.Get(string(UserIDKey))
	if !exists {
		return "", false
	}

	id, ok := userID.(string)
	return id, ok
}
