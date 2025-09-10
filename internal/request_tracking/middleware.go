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
			// Pro: enforce ProDailyTokens by today's token usage.
			if isPro, _, err := trackingService.HasActivePro(c.Request.Context(), userID); err == nil && isPro {
				log.Debug("checking pro daily tokens limit")
				used, uerr := trackingService.GetUserTokenUsageToday(c.Request.Context(), userID)
				if uerr != nil {
					log.Error("failed to get today's token usage", slog.String("error", uerr.Error()))
				} else if used >= config.AppConfig.ProDailyTokens {
					if !config.AppConfig.RateLimitLogOnly {
						c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{
							"error": "Pro daily token limit exceeded.",
							"tier":  "pro",
							"limit": config.AppConfig.ProDailyTokens,
						})
						return
					}
					log.Warn("pro daily tokens limit exceeded", slog.Int64("limit", config.AppConfig.ProDailyTokens))
				}
			} else {
				// Free tier: allow until FreeLifetimeTokens lifetime tokens.
				log.Debug("checking free lifetime tokens limit")
				lifetime, lerr := trackingService.GetUserLifetimeTokenUsage(c.Request.Context(), userID)
				if lerr != nil {
					log.Error("failed to get lifetime token usage", slog.String("error", lerr.Error()))
				} else if lifetime >= config.AppConfig.FreeLifetimeTokens {
					// Drip tier: enforce daily messages after free lifetime is exhausted.
					log.Debug("checking drip daily messages limit")
					reqs, derr := trackingService.GetUserRequestCountToday(c.Request.Context(), userID)
					if derr != nil {
						log.Error("failed to get request count today", slog.String("error", derr.Error()))
					} else if reqs >= config.AppConfig.DripDailyMessages {
						if !config.AppConfig.RateLimitLogOnly {
							c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{
								"error": "Daily limit exceeded. Please try again tomorrow or upgrade.",
								"tier":  "drip",
								"limit": config.AppConfig.DripDailyMessages,
							})
							return
						}
						log.Warn("drip limit exceeded", slog.Int64("limit", config.AppConfig.DripDailyMessages))
					}
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

		c.Next()
	}
}
