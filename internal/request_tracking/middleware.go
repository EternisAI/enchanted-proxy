package request_tracking

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"

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

// replaceModelInRequestBody replaces the model field in a JSON request body.
// Returns the original body if replacement fails.
func replaceModelInRequestBody(body []byte, newModel string) []byte {
	if len(body) == 0 {
		return body
	}

	var req map[string]interface{}
	if err := json.Unmarshal(body, &req); err != nil {
		return body
	}

	req["model"] = newModel
	newBody, err := json.Marshal(req)
	if err != nil {
		return body
	}

	return newBody
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

			// Extract model and check access/limits
			model := extractModelFromRequestBody(c.Request.URL.Path, requestBody)
			inFallbackMode := false
			originalModel := model

			if model != "" {
				// Check primary model access
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

			// Check primary quota limits
			// Check monthly quota
			if tierConfig.MonthlyPlanTokens > 0 {
				used, err := trackingService.GetUserPlanTokensThisMonth(c.Request.Context(), userID)
				if err != nil {
					log.Error("failed to get monthly plan token usage",
						slog.String("error", err.Error()),
						slog.String("user_id", userID))
				} else if used >= tierConfig.MonthlyPlanTokens {
					if tierConfig.FallbackConfig != nil {
						inFallbackMode = true
						log.Info("monthly limit exceeded, entering fallback mode",
							slog.String("user_id", userID),
							slog.String("tier", tierConfig.Name))
					} else {
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
			}

			// Check weekly quota
			if !inFallbackMode && tierConfig.WeeklyPlanTokens > 0 {
				used, err := trackingService.GetUserPlanTokensThisWeek(c.Request.Context(), userID)
				if err != nil {
					log.Error("failed to get weekly plan token usage",
						slog.String("error", err.Error()),
						slog.String("user_id", userID))
				} else if used >= tierConfig.WeeklyPlanTokens {
					if tierConfig.FallbackConfig != nil {
						inFallbackMode = true
						log.Info("weekly limit exceeded, entering fallback mode",
							slog.String("user_id", userID),
							slog.String("tier", tierConfig.Name))
					} else {
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
			}

			// Check daily quota
			if !inFallbackMode && tierConfig.DailyPlanTokens > 0 {
				used, err := trackingService.GetUserPlanTokensToday(c.Request.Context(), userID)
				if err != nil {
					log.Error("failed to get daily plan token usage",
						slog.String("error", err.Error()),
						slog.String("user_id", userID))
				} else if used >= tierConfig.DailyPlanTokens {
					if tierConfig.FallbackConfig != nil {
						inFallbackMode = true
						log.Info("daily limit exceeded, entering fallback mode",
							slog.String("user_id", userID),
							slog.String("tier", tierConfig.Name))
					} else {
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

			// Handle fallback mode
			if inFallbackMode {
				fallbackCfg := tierConfig.FallbackConfig

				// Check fallback quota
				if fallbackCfg.DailyFallbackTokens > 0 {
					used, err := trackingService.GetUserFallbackTokensToday(c.Request.Context(), userID)
					if err != nil {
						log.Error("failed to get fallback token usage",
							slog.String("error", err.Error()),
							slog.String("user_id", userID))
					} else if used >= fallbackCfg.DailyFallbackTokens {
						c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{
							"error":     fmt.Sprintf("%s fallback limit exceeded", tierConfig.DisplayName),
							"tier":      tierConfig.Name,
							"limit":     fallbackCfg.DailyFallbackTokens,
							"used":      used,
							"resets_at": tierConfig.GetDailyResetTime(),
						})
						return
					}
				}

				// Auto-route to fallback model if current model not allowed
				if model != "" && !fallbackCfg.IsModelAllowed(model) {
					fallbackModel := fallbackCfg.GetFirstAllowedModel()
					if fallbackModel == "" {
						c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{
							"error": "No fallback models configured",
						})
						return
					}

					// Replace model in request body
					newBody := replaceModelInRequestBody(requestBody, fallbackModel)
					c.Request.Body = newReaderCloser(newBody)
					model = fallbackModel

					log.Info("auto-routed to fallback model",
						slog.String("user_id", userID),
						slog.String("requested_model", originalModel),
						slog.String("fallback_model", fallbackModel))
				}

				// Set context flag for fallback mode
				c.Set("isFallbackRequest", true)
			} else {
				// Primary mode
				c.Set("isFallbackRequest", false)
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
