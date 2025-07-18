package auth

import (
	"context"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

const (
	UserUUIDKey = "user_uuid"
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

		// Attach user UUID to context
		c.Set(UserUUIDKey, userUUID)
		c.Next()
	}
}

// RequireAuthHTTP is a Chi-compatible middleware that validates Firebase tokens and attaches user UUID to context.
func (f *FirebaseAuthMiddleware) RequireAuthHTTP() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Extract Authorization header
			authHeader := r.Header.Get("Authorization")
			if authHeader == "" {
				http.Error(w, `{"error": "Authorization header is required"}`, http.StatusUnauthorized)
				return
			}

			// Check if it's a Bearer token
			if !strings.HasPrefix(authHeader, "Bearer ") {
				http.Error(w, `{"error": "Authorization header must be a Bearer token"}`, http.StatusUnauthorized)
				return
			}

			// Extract the token
			token := strings.TrimPrefix(authHeader, "Bearer ")
			if token == "" {
				http.Error(w, `{"error": "Bearer token is empty"}`, http.StatusUnauthorized)
				return
			}

			// Validate the token with Firebase
			userUUID, err := f.validator.ValidateToken(token)
			if err != nil {
				http.Error(w, `{"error": "Invalid or expired token"}`, http.StatusUnauthorized)
				return
			}

			// Attach user UUID to request context
			ctx := context.WithValue(r.Context(), UserUUIDKey, userUUID)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// GetUserUUID extracts the user UUID from the Gin context.
func GetUserUUID(c *gin.Context) (string, bool) {
	userUUID, exists := c.Get(UserUUIDKey)
	if !exists {
		return "", false
	}

	uuid, ok := userUUID.(string)
	return uuid, ok
}

// GetUserUUIDFromHTTPContext extracts the user UUID from the HTTP request context.
func GetUserUUIDFromHTTPContext(ctx context.Context) (string, bool) {
	userUUID := ctx.Value(UserUUIDKey)
	if userUUID == nil {
		return "", false
	}

	uuid, ok := userUUID.(string)
	return uuid, ok
}
