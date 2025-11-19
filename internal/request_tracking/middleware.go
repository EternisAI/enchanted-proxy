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
		userID, exists := auth.GetUserID(c)
		if !exists {
			c.Next()
			return
		}

		log := logger.WithContext(c.Request.Context()).WithComponent("request_tracking")

		if config.AppConfig.RateLimitEnabled {
			// Check pro status with explicit error handling
			isPro, _, err := trackingService.HasActivePro(c.Request.Context(), userID)
			if err != nil {
				// CRITICAL: If we can't check pro status (DB error, network issue, etc.),
				// fail open and allow the request rather than incorrectly applying free tier limits.
				// This prevents pro subscribers from being rate limited due to transient errors.
				log.Error("failed to check pro status - allowing request to proceed",
					slog.String("error", err.Error()),
					slog.String("user_id", userID))
				c.Next()
				return
			}

			// Pro: enforce ProDailyTokens by today's token usage.
			if isPro {
				log.Debug("checking pro daily tokens limit", slog.String("user_id", userID))
				used, uerr := trackingService.GetUserTokenUsageToday(c.Request.Context(), userID)
				if uerr != nil {
					log.Error("failed to get today's token usage",
						slog.String("error", uerr.Error()),
						slog.String("user_id", userID))
				} else if used >= config.AppConfig.ProDailyTokens {
					log.Warn("rate limit exceeded - returning 429",
						slog.String("user_id", userID),
						slog.String("tier", "pro"),
						slog.Int64("limit", config.AppConfig.ProDailyTokens),
						slog.Int64("used", used),
						slog.Bool("log_only", config.AppConfig.RateLimitLogOnly))
					if !config.AppConfig.RateLimitLogOnly {
						c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{
							"error": "Pro daily token limit exceeded.",
							"tier":  "pro",
							"limit": config.AppConfig.ProDailyTokens,
						})
						return
					}
				}
			} else {
				// Free tier: allow until FreeLifetimeTokens lifetime tokens.
				log.Debug("checking free lifetime tokens limit", slog.String("user_id", userID))
				lifetime, lerr := trackingService.GetUserLifetimeTokenUsage(c.Request.Context(), userID)
				if lerr != nil {
					log.Error("failed to get lifetime token usage",
						slog.String("error", lerr.Error()),
						slog.String("user_id", userID))
				} else if lifetime >= config.AppConfig.FreeLifetimeTokens {
					// Drip tier: enforce daily messages after free lifetime is exhausted.
					log.Debug("checking drip daily messages limit", slog.String("user_id", userID))
					reqs, derr := trackingService.GetUserRequestCountToday(c.Request.Context(), userID)
					if derr != nil {
						log.Error("failed to get request count today",
							slog.String("error", derr.Error()),
							slog.String("user_id", userID))
					} else if reqs >= config.AppConfig.DripDailyMessages {
						log.Warn("rate limit exceeded - returning 429",
							slog.String("user_id", userID),
							slog.String("tier", "drip"),
							slog.Int64("limit", config.AppConfig.DripDailyMessages),
							slog.Int64("used", reqs),
							slog.Int64("lifetime_tokens", lifetime),
							slog.Bool("log_only", config.AppConfig.RateLimitLogOnly))
						if !config.AppConfig.RateLimitLogOnly {
							c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{
								"error": "Daily limit exceeded. Please try again tomorrow or upgrade.",
								"tier":  "drip",
								"limit": config.AppConfig.DripDailyMessages,
							})
							return
						}
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
