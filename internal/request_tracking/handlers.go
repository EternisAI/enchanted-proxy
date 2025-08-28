package request_tracking

import (
	"net/http"
	"time"

	"github.com/eternisai/enchanted-proxy/internal/auth"
	"github.com/eternisai/enchanted-proxy/internal/config"
	"github.com/gin-gonic/gin"
)

// RateLimitStatusHandler returns the current rate limit status for the authenticated user.
func RateLimitStatusHandler(trackingService *Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		userID, exists := auth.GetUserUUID(c)
		if !exists {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "User not authenticated"})
			return
		}

		if !config.AppConfig.RateLimitEnabled {
			c.JSON(http.StatusOK, gin.H{
				"enabled": false,
				"message": "Rate limiting is disabled",
			})
			return
		}

		isUnderLimit, err := trackingService.CheckRateLimit(c.Request.Context(), userID, config.AppConfig.RateLimitTokensPerDay)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to check rate limit"})
			return
		}

		// Get current request count for the user in the last day.
		now := time.Now().UTC()
		dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)

		tokenUsage, err := trackingService.GetUserTokenUsageSince(c.Request.Context(), userID, dayStart)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to get token usage"})
			return
		}

		remaining := config.AppConfig.RateLimitTokensPerDay - tokenUsage
		if remaining < 0 {
			remaining = 0
		}

		c.JSON(http.StatusOK, gin.H{
			"enabled":       config.AppConfig.RateLimitEnabled,
			"limit":         config.AppConfig.RateLimitTokensPerDay,
			"used":          tokenUsage,
			"remaining":     remaining,
			"under_limit":   isUnderLimit,
			"log_only_mode": config.AppConfig.RateLimitLogOnly,
		})
	}
}
