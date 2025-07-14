package request_tracking

import (
	"net/http"
	"time"

	"github.com/eternisai/enchanted-proxy/pkg/auth"
	"github.com/eternisai/enchanted-proxy/pkg/config"
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

		isUnderLimit, err := trackingService.CheckRateLimit(c.Request.Context(), userID, config.AppConfig.RateLimitRequestsPerDay)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to check rate limit"})
			return
		}

		// Get current request count for the user in the last day
		oneDayAgo := time.Now().Add(-24 * time.Hour)
		requestCount, err := trackingService.GetUserRequestCountSince(c.Request.Context(), userID, oneDayAgo)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to get request count"})
			return
		}

		c.JSON(http.StatusOK, gin.H{
			"enabled":       config.AppConfig.RateLimitEnabled,
			"limit":         config.AppConfig.RateLimitRequestsPerDay,
			"current_count": requestCount,
			"remaining":     config.AppConfig.RateLimitRequestsPerDay - requestCount,
			"under_limit":   isUnderLimit,
			"log_only_mode": config.AppConfig.RateLimitLogOnly,
		})
	}
}
