package mcp

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

// MCPAuthMiddleware checks if the request is authorized.
func MCPAuthMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		authHeader := c.GetHeader("Authorization")
		if authHeader == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "Authorization header required for /mcp endpoints"})
			return
		}

		parts := strings.Split(authHeader, " ")
		if len(parts) != 2 || strings.ToLower(parts[0]) != "bearer" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "Invalid Authorization header format"})
			return
		}
		tokenString := parts[1]

		userInfoURL := "https://www.googleapis.com/oauth2/v3/userinfo"
		req, err := http.NewRequestWithContext(c.Request.Context(), "GET", userInfoURL, nil)
		if err != nil {
			log.Printf("Failed to create request to userinfo endpoint: %v\n", err)
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "Failed to create token validation request"})
			return
		}
		req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", tokenString))
		log.Printf("Making request")
		client := &http.Client{}
		resp, err := client.Do(req)
		if err != nil {
			log.Printf("Failed to call userinfo endpoint: %v\n", err)
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "Token validation failed"})
			return
		}
		defer func() {
			if err := resp.Body.Close(); err != nil {
				log.Printf("Error closing response body: %v", err)
			}
		}()
		log.Printf("Userinfo endpoint returned status: %d\n", resp.StatusCode)
		if resp.StatusCode != http.StatusOK {
			log.Printf("Userinfo endpoint returned non-200 status: %d\n", resp.StatusCode)
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "Invalid token"})
			return
		}

		var userInfo map[string]interface{}
		if err := json.NewDecoder(resp.Body).Decode(&userInfo); err != nil {
			log.Printf("Failed to decode userinfo response: %v\n", err)
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "Failed to parse token validation response"})
			return
		}
		log.Printf("Userinfo response: %v\n", userInfo)
		if userInfo["email"] == nil || userInfo["email"] == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "Invalid token"})
			return
		}
		log.Printf("Response validated")
		c.Next()
	}
}
