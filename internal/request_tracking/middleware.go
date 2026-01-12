package request_tracking

import (
	"bytes"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/eternisai/enchanted-proxy/internal/auth"
	"github.com/eternisai/enchanted-proxy/internal/common"
	"github.com/eternisai/enchanted-proxy/internal/config"
	"github.com/eternisai/enchanted-proxy/internal/errors"
	"github.com/eternisai/enchanted-proxy/internal/logger"
	"github.com/gin-gonic/gin"
)

// newReaderCloser creates an io.ReadCloser from a byte slice.
func newReaderCloser(b []byte) io.ReadCloser {
	return io.NopCloser(bytes.NewReader(b))
}

// extractModelFromRequestBody extracts the model field from request body bytes.
// Delegates to common package for consistent implementation across codebase.
func extractModelFromRequestBody(path string, body []byte) string {
	return common.ExtractModelFromRequestBody(path, body)
}

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
			// Get user's tier and config
			tierConfig, expiresAt, err := trackingService.GetUserTierConfig(c.Request.Context(), userID)
			if err != nil {
				log.Error("failed to get tier config",
					slog.String("error", err.Error()),
					slog.String("user_id", userID))

				// Fail closed if configured (prevents rate limit bypass during DB outage)
				if config.AppConfig.RateLimitFailClosed {
					c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{
						"error": "Rate limit service temporarily unavailable",
					})
					return
				}

				// Otherwise fail open (allow request)
				log.Warn("allowing request despite tier config error (fail open mode)")
				c.Next()
				return
			}

			log.Debug("checking rate limits for user",
				slog.String("user_id", userID),
				slog.String("tier", tierConfig.Name),
				slog.Int64("monthly_limit", tierConfig.MonthlyPlanTokens),
				slog.Int64("daily_limit", tierConfig.DailyPlanTokens))

			// Read request body once for model extraction
			var requestBody []byte
			if c.Request.Body != nil {
				var err error
				requestBody, err = io.ReadAll(c.Request.Body)
				if err == nil {
					// Restore body for downstream handlers
					c.Request.Body = newReaderCloser(requestBody)
				}
			}

			// Model access control
			model := extractModelFromRequestBody(c.Request.URL.Path, requestBody)
			if model != "" {
				if !tierConfig.IsModelAllowed(model) {
					log.Warn("model access denied",
						slog.String("user_id", userID),
						slog.String("model", model),
						slog.String("tier", tierConfig.Name))

					err := errors.ModelNotAllowed(model, tierConfig.Name, tierConfig.DisplayName, tierConfig.AllowedModels)
					errors.AbortWithForbidden(c, err)
					return
				}
			}

			// Check monthly quota (if configured)
			if tierConfig.MonthlyPlanTokens > 0 {
				used, err := trackingService.GetUserPlanTokensThisMonth(c.Request.Context(), userID)
				if err != nil {
					log.Error("failed to get monthly plan token usage",
						slog.String("error", err.Error()),
						slog.String("user_id", userID))
				} else if used >= tierConfig.MonthlyPlanTokens {
					log.Warn("monthly rate limit exceeded",
						slog.String("user_id", userID),
						slog.String("tier", tierConfig.Name),
						slog.Int64("limit", tierConfig.MonthlyPlanTokens),
						slog.Int64("used", used))
					c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{
						"error":     fmt.Sprintf("%s monthly plan token limit exceeded", tierConfig.DisplayName),
						"tier":      tierConfig.Name,
						"limit":     tierConfig.MonthlyPlanTokens,
						"used":      used,
						"resets_at": tierConfig.GetMonthlyResetTime(),
					})
					return
				}
			}

			// Check weekly quota (if configured)
			if tierConfig.WeeklyPlanTokens > 0 {
				used, err := trackingService.GetUserPlanTokensThisWeek(c.Request.Context(), userID)
				if err != nil {
					log.Error("failed to get weekly plan token usage",
						slog.String("error", err.Error()),
						slog.String("user_id", userID))
				} else if used >= tierConfig.WeeklyPlanTokens {
					log.Warn("weekly rate limit exceeded",
						slog.String("user_id", userID),
						slog.String("tier", tierConfig.Name),
						slog.Int64("limit", tierConfig.WeeklyPlanTokens),
						slog.Int64("used", used))
					c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{
						"error":     fmt.Sprintf("%s weekly plan token limit exceeded", tierConfig.DisplayName),
						"tier":      tierConfig.Name,
						"limit":     tierConfig.WeeklyPlanTokens,
						"used":      used,
						"resets_at": tierConfig.GetWeeklyResetTime(),
					})
					return
				}
			}

			// Check daily quota (if configured)
			if tierConfig.DailyPlanTokens > 0 {
				used, err := trackingService.GetUserPlanTokensToday(c.Request.Context(), userID)
				if err != nil {
					log.Error("failed to get daily plan token usage",
						slog.String("error", err.Error()),
						slog.String("user_id", userID))
				} else if used >= tierConfig.DailyPlanTokens {
					// Normal quota exceeded - check if fallback is available
					isFallbackModel := tierConfig.IsFallbackModel(model)
					hasFallback := tierConfig.FallbackDailyPlanTokens > 0

					if hasFallback && isFallbackModel {
						// User is requesting a fallback model - check fallback quota
						fallbackUsed, fallbackErr := trackingService.GetUserFallbackPlanTokensToday(c.Request.Context(), userID, tierConfig.FallbackModel)
						if fallbackErr != nil {
							log.Error("failed to get fallback plan token usage",
								slog.String("error", fallbackErr.Error()),
								slog.String("user_id", userID))
						} else if fallbackUsed >= tierConfig.FallbackDailyPlanTokens {
							// Fallback quota also exceeded - hard limit
							log.Warn("fallback rate limit exceeded (hard limit)",
								slog.String("user_id", userID),
								slog.String("tier", tierConfig.Name),
								slog.Int64("fallback_limit", tierConfig.FallbackDailyPlanTokens),
								slog.Int64("fallback_used", fallbackUsed))
							c.Header("X-Rate-Limit-Type", "hard")
							c.Header("X-Rate-Limit-Resets-At", tierConfig.GetDailyResetTime().Format(time.RFC3339))
							c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{
								"error":     fmt.Sprintf("%s daily fallback limit exceeded", tierConfig.DisplayName),
								"tier":      tierConfig.Name,
								"limit":     tierConfig.FallbackDailyPlanTokens,
								"used":      fallbackUsed,
								"resets_at": tierConfig.GetDailyResetTime(),
							})
							return
						}
						// Fallback quota available - allow request to proceed
						log.Info("using fallback quota",
							slog.String("user_id", userID),
							slog.String("model", model),
							slog.Int64("fallback_used", fallbackUsed),
							slog.Int64("fallback_limit", tierConfig.FallbackDailyPlanTokens))
					} else if hasFallback && !isFallbackModel {
						// Soft limit - user should switch to fallback model
						log.Warn("daily rate limit exceeded (soft limit)",
							slog.String("user_id", userID),
							slog.String("tier", tierConfig.Name),
							slog.Int64("limit", tierConfig.DailyPlanTokens),
							slog.Int64("used", used),
							slog.String("model", model))
						c.Header("X-Rate-Limit-Type", "soft")
						c.Header("X-Rate-Limit-Resets-At", tierConfig.GetDailyResetTime().Format(time.RFC3339))
						c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{
							"error":     fmt.Sprintf("%s daily plan token limit exceeded", tierConfig.DisplayName),
							"tier":      tierConfig.Name,
							"limit":     tierConfig.DailyPlanTokens,
							"used":      used,
							"resets_at": tierConfig.GetDailyResetTime(),
						})
						return
					} else {
						// No fallback available (free tier) - hard limit
						log.Warn("daily rate limit exceeded (hard limit, no fallback)",
							slog.String("user_id", userID),
							slog.String("tier", tierConfig.Name),
							slog.Int64("limit", tierConfig.DailyPlanTokens),
							slog.Int64("used", used))
						c.Header("X-Rate-Limit-Type", "hard")
						c.Header("X-Rate-Limit-Resets-At", tierConfig.GetDailyResetTime().Format(time.RFC3339))
						c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{
							"error":     fmt.Sprintf("%s daily plan token limit exceeded", tierConfig.DisplayName),
							"tier":      tierConfig.Name,
							"limit":     tierConfig.DailyPlanTokens,
							"used":      used,
							"resets_at": tierConfig.GetDailyResetTime(),
						})
						return
					}
				}
			}

			// Store tier config in context for later use
			c.Set("tierConfig", tierConfig)
			if expiresAt != nil {
				c.Set("tierExpiresAt", *expiresAt)
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
