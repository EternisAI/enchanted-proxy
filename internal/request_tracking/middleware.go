package request_tracking

import (
	"log/slog"
	"net/http"

	"github.com/eternisai/enchanted-proxy/internal/auth"
	"github.com/eternisai/enchanted-proxy/internal/config"
	"github.com/eternisai/enchanted-proxy/internal/logger"
	"github.com/gin-gonic/gin"
)

// RequestTrackingMiddleware logs requests for authenticated users and checks rate limits.
func RequestTrackingMiddleware(trackingService *Service, logger *logger.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		userID, exists := auth.GetUserUUID(c)
		if !exists {
			c.Next()
			return
		}

		log := logger.WithContext(c.Request.Context()).WithComponent("request_tracking")

		if config.AppConfig.RateLimitEnabled {
			isUnderLimit, err := trackingService.CheckRateLimit(c.Request.Context(), userID, config.AppConfig.RateLimitRequestsPerDay)
			if err != nil {
				log.Error("failed to check rate limit", slog.String("error", err.Error()))
			} else if !isUnderLimit {
				log.Warn("rate limit exceeded", slog.Int64("limit", config.AppConfig.RateLimitRequestsPerDay))

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

		log.Info("processing request",
			slog.String("endpoint", endpoint),
			slog.String("provider", provider),
			slog.String("base_url", baseURL),
			slog.String("method", c.Request.Method))

		info := RequestInfo{
			UserID:   userID,
			Endpoint: endpoint,
			Model:    "", // Not extracting model initially.
			Provider: provider,
		}

		if err := trackingService.LogRequestAsync(c.Request.Context(), info); err != nil {
			log.Error("failed to queue request log", slog.String("error", err.Error()))
		}

		c.Next()
	}
}
