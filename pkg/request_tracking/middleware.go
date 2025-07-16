package request_tracking

import (
	"net/http"

	"github.com/charmbracelet/log"
	"github.com/eternisai/enchanted-proxy/pkg/auth"
	"github.com/eternisai/enchanted-proxy/pkg/config"
	"github.com/gin-gonic/gin"
)

// RequestTrackingMiddleware logs requests for authenticated users and checks rate limits.
func RequestTrackingMiddleware(trackingService *Service, logger *log.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		userID, exists := auth.GetUserUUID(c)
		if !exists {
			c.Next()
			return
		}

		if config.AppConfig.RateLimitEnabled {
			isUnderLimit, err := trackingService.CheckRateLimit(c.Request.Context(), userID, config.AppConfig.RateLimitRequestsPerDay)
			if err != nil {
				logger.Error("Failed to check rate limit", "user_id", userID, "error", err)
			} else if !isUnderLimit {
				logger.Warn("🚨 RATE LIMIT EXCEEDED", "user_id", userID, "limit", config.AppConfig.RateLimitRequestsPerDay)

				if !config.AppConfig.RateLimitLogOnly {
					c.JSON(http.StatusTooManyRequests, gin.H{
						"error": "Rate limit exceeded. Please try again later.",
						"limit": config.AppConfig.RateLimitRequestsPerDay,
					})
					return
				}
			}
		}

		baseURL := c.GetHeader("X-BASE-URL")
		provider := GetProviderFromBaseURL(baseURL)

		endpoint := c.Request.URL.Path

		info := RequestInfo{
			UserID:   userID,
			Endpoint: endpoint,
			Model:    "", // Not extracting model initially.
			Provider: provider,
		}

		if err := trackingService.LogRequestAsync(c.Request.Context(), info); err != nil {
			logger.Error("Failed to queue request log", "error", err)
		}

		c.Next()
	}
}
