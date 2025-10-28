package request_tracking

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/eternisai/enchanted-proxy/internal/auth"
	"github.com/eternisai/enchanted-proxy/internal/config"
	"github.com/eternisai/enchanted-proxy/internal/logger"
	"github.com/gin-gonic/gin"
)

// RateLimitStatusHandler returns the current rate limit status for the authenticated user.
func RateLimitStatusHandler(trackingService *Service, log *logger.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		userID, exists := auth.GetUserUUID(c)
		if !exists {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "User not authenticated"})
			return
		}

		reqLog := log.WithContext(c.Request.Context()).WithComponent("rate_limit_status")

		if !config.AppConfig.RateLimitEnabled {
			c.JSON(http.StatusOK, gin.H{
				"enabled": false,
				"message": "Rate limiting is disabled",
			})
			return
		}

		now := time.Now().UTC()
		dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)

		// Pro tier.
		isPro, _, err := trackingService.HasActivePro(c.Request.Context(), userID)
		if err != nil {
			reqLog.Error("failed to check pro status in rate limit status endpoint",
				slog.String("error", err.Error()),
				slog.String("user_id", userID))
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to get active pro"})
			return
		}
		if isPro {
			used, err := trackingService.GetUserTokenUsageToday(c.Request.Context(), userID)
			if err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to get token usage"})
				return
			}
			limit := config.AppConfig.ProDailyTokens
			remaining := limit - used
			if remaining < 0 {
				remaining = 0
			}
			c.JSON(http.StatusOK, gin.H{
				"enabled":       config.AppConfig.RateLimitEnabled,
				"tier":          "pro",
				"limit":         limit,
				"used":          used,
				"remaining":     remaining,
				"resets_at":     dayStart.Add(24 * time.Hour),
				"under_limit":   used < limit,
				"log_only_mode": config.AppConfig.RateLimitLogOnly,
			})
			return
		}

		// Free tier (lifetime).
		lifetime, err := trackingService.GetUserLifetimeTokenUsage(c.Request.Context(), userID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to get lifetime usage"})
			return
		}
		if lifetime < config.AppConfig.FreeLifetimeTokens {
			limit := config.AppConfig.FreeLifetimeTokens
			used := lifetime
			remaining := limit - used
			if remaining < 0 {
				remaining = 0
			}
			c.JSON(http.StatusOK, gin.H{
				"enabled":       config.AppConfig.RateLimitEnabled,
				"tier":          "free",
				"limit":         limit,
				"used":          used,
				"remaining":     remaining,
				"resets_at":     nil,
				"under_limit":   used < limit,
				"log_only_mode": config.AppConfig.RateLimitLogOnly,
			})
			return
		}

		// Drip tier (daily requests).
		reqs, err := trackingService.GetUserRequestCountToday(c.Request.Context(), userID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to get request count"})
			return
		}
		limit := config.AppConfig.DripDailyMessages
		remaining := limit - reqs
		if remaining < 0 {
			remaining = 0
		}
		c.JSON(http.StatusOK, gin.H{
			"enabled":       config.AppConfig.RateLimitEnabled,
			"tier":          "drip",
			"limit":         limit,
			"used":          reqs,
			"remaining":     remaining,
			"resets_at":     dayStart.Add(24 * time.Hour),
			"under_limit":   reqs < limit,
			"log_only_mode": config.AppConfig.RateLimitLogOnly,
		})
	}
}
