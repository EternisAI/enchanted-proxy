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
		userID, exists := auth.GetUserID(c)
		if !exists {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "User not authenticated"})
			return
		}

		reqLog := log.WithContext(c.Request.Context()).WithComponent("rate_limit_status")

		// Get subscription provider (works regardless of rate limit being enabled)
		provider, provErr := trackingService.GetSubscriptionProvider(c.Request.Context(), userID)
		if provErr != nil {
			reqLog.Error("failed to get subscription provider",
				slog.String("error", provErr.Error()),
				slog.String("user_id", userID))
			// Don't fail the request, just omit the provider field
			provider = ""
		}

		// Check Pro status for all tiers (needed for pro_expires_at field)
		isPro, proExpiresAt, err := trackingService.HasActivePro(c.Request.Context(), userID)
		if err != nil {
			reqLog.Error("failed to check pro status in rate limit status endpoint",
				slog.String("error", err.Error()),
				slog.String("user_id", userID))
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to get active pro"})
			return
		}

		if !config.AppConfig.RateLimitEnabled {
			response := gin.H{
				"enabled":       false,
				"tier":          "unlimited",
				"limit":         0,
				"used":          0,
				"remaining":     0,
				"resets_at":     nil,
				"under_limit":   true,
				"log_only_mode": false,
			}
			if provider != "" {
				response["subscription_provider"] = provider
			}
			if proExpiresAt != nil {
				response["pro_expires_at"] = proExpiresAt
			}
			c.JSON(http.StatusOK, response)
			return
		}

		now := time.Now().UTC()
		dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)

		// Pro tier.
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
			response := gin.H{
				"enabled":       config.AppConfig.RateLimitEnabled,
				"tier":          "pro",
				"limit":         limit,
				"used":          used,
				"remaining":     remaining,
				"resets_at":     dayStart.Add(24 * time.Hour),
				"under_limit":   used < limit,
				"log_only_mode": config.AppConfig.RateLimitLogOnly,
			}
			if provider != "" {
				response["subscription_provider"] = provider
			}
			if proExpiresAt != nil {
				response["pro_expires_at"] = proExpiresAt
			}
			c.JSON(http.StatusOK, response)
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
			response := gin.H{
				"enabled":       config.AppConfig.RateLimitEnabled,
				"tier":          "free",
				"limit":         limit,
				"used":          used,
				"remaining":     remaining,
				"resets_at":     nil,
				"under_limit":   used < limit,
				"log_only_mode": config.AppConfig.RateLimitLogOnly,
			}
			if provider != "" {
				response["subscription_provider"] = provider
			}
			if proExpiresAt != nil {
				response["pro_expires_at"] = proExpiresAt
			}
			c.JSON(http.StatusOK, response)
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
		response := gin.H{
			"enabled":       config.AppConfig.RateLimitEnabled,
			"tier":          "drip",
			"limit":         limit,
			"used":          reqs,
			"remaining":     remaining,
			"resets_at":     dayStart.Add(24 * time.Hour),
			"under_limit":   reqs < limit,
			"log_only_mode": config.AppConfig.RateLimitLogOnly,
		}
		if provider != "" {
			response["subscription_provider"] = provider
		}
		if proExpiresAt != nil {
			response["pro_expires_at"] = proExpiresAt
		}
		c.JSON(http.StatusOK, response)
	}
}
