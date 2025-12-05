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

// RateLimitStatusResponse represents the comprehensive rate limit status response.
type RateLimitStatusResponse struct {
	// Core status
	Enabled             bool   `json:"enabled"`
	Tier                string `json:"tier"`
	TierDisplay         string `json:"tier_display"`
	RateLimitingEnabled bool   `json:"rate_limiting_enabled"`

	// Token limits
	MonthlyTokens *TokenLimitInfo `json:"monthly_tokens,omitempty"`
	WeeklyTokens  *TokenLimitInfo `json:"weekly_tokens,omitempty"`
	DailyTokens   *TokenLimitInfo `json:"daily_tokens,omitempty"`

	// Subscription info
	SubscriptionProvider string     `json:"subscription_provider,omitempty"`
	ExpiresAt            *time.Time `json:"expires_at,omitempty"`

	// Model access (empty = all models allowed, non-empty = only these models allowed)
	AllowedModels []string `json:"allowed_models,omitempty"`

	// Deep research limits
	DeepResearch *DeepResearchInfo `json:"deep_research"`

	// Allowed features (empty = all features allowed, non-empty = only these features allowed)
	AllowedFeatures []string `json:"allowed_features"`
}

type TokenLimitInfo struct {
	Limit      int64     `json:"limit"`
	Used       int64     `json:"used"`
	Remaining  int64     `json:"remaining"`
	ResetsAt   time.Time `json:"resets_at"`
	UnderLimit bool      `json:"under_limit"`
	Percentage float64   `json:"percentage"` // Used percentage (0-100)
}

type DeepResearchInfo struct {
	DailyRuns             int `json:"daily_runs"`               // -1 = unlimited
	LifetimeRuns          int `json:"lifetime_runs"`            // 0 = check daily only
	TokenCap              int `json:"token_cap"`                // Per-run cap
	MaxActiveSessions     int `json:"max_active_sessions"`
	DailyRunsUsed         int `json:"daily_runs_used"`
	LifetimeRunsUsed      int `json:"lifetime_runs_used"`
}

// RateLimitStatusHandler returns comprehensive rate limit and tier information.
func RateLimitStatusHandler(trackingService *Service, log *logger.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		userID, exists := auth.GetUserID(c)
		if !exists {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "User not authenticated"})
			return
		}

		reqLog := log.WithContext(c.Request.Context()).WithComponent("rate_limit_status")
		ctx := c.Request.Context()

		// Get user's tier configuration
		tierConfig, expiresAt, err := trackingService.GetUserTierConfig(ctx, userID)
		if err != nil {
			reqLog.Error("failed to get tier config",
				slog.String("error", err.Error()),
				slog.String("user_id", userID))
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to get tier information"})
			return
		}

		// Get subscription provider
		provider, _ := trackingService.GetSubscriptionProvider(ctx, userID)

		// Convert allowed features to strings
		allowedFeatures := make([]string, len(tierConfig.AllowedFeatures))
		for i, feature := range tierConfig.AllowedFeatures {
			allowedFeatures[i] = string(feature)
		}

		// Build response
		response := RateLimitStatusResponse{
			Enabled:              config.AppConfig.RateLimitEnabled,
			Tier:                 tierConfig.Name,
			TierDisplay:          tierConfig.DisplayName,
			RateLimitingEnabled:  config.AppConfig.RateLimitEnabled,
			SubscriptionProvider: provider,
			ExpiresAt:            expiresAt,
			AllowedModels:        tierConfig.AllowedModels,
			AllowedFeatures:      allowedFeatures,
		}

		// Monthly token limit (if configured)
		if tierConfig.MonthlyPlanTokens > 0 {
			used, err := trackingService.GetUserPlanTokensThisMonth(ctx, userID)
			if err != nil {
				reqLog.Error("failed to get monthly usage", slog.String("error", err.Error()))
				used = 0
			}
			remaining := tierConfig.MonthlyPlanTokens - used
			if remaining < 0 {
				remaining = 0
			}
			percentage := (float64(used) / float64(tierConfig.MonthlyPlanTokens)) * 100
			response.MonthlyTokens = &TokenLimitInfo{
				Limit:      tierConfig.MonthlyPlanTokens,
				Used:       used,
				Remaining:  remaining,
				ResetsAt:   tierConfig.GetMonthlyResetTime(),
				UnderLimit: used < tierConfig.MonthlyPlanTokens,
				Percentage: percentage,
			}
		}

		// Weekly token limit (if configured)
		if tierConfig.WeeklyPlanTokens > 0 {
			used, err := trackingService.GetUserPlanTokensThisWeek(ctx, userID)
			if err != nil {
				reqLog.Error("failed to get weekly usage", slog.String("error", err.Error()))
				used = 0
			}
			remaining := tierConfig.WeeklyPlanTokens - used
			if remaining < 0 {
				remaining = 0
			}
			percentage := (float64(used) / float64(tierConfig.WeeklyPlanTokens)) * 100
			response.WeeklyTokens = &TokenLimitInfo{
				Limit:      tierConfig.WeeklyPlanTokens,
				Used:       used,
				Remaining:  remaining,
				ResetsAt:   tierConfig.GetWeeklyResetTime(),
				UnderLimit: used < tierConfig.WeeklyPlanTokens,
				Percentage: percentage,
			}
		}

		// Daily token limit (if configured)
		if tierConfig.DailyPlanTokens > 0 {
			used, err := trackingService.GetUserPlanTokensToday(ctx, userID)
			if err != nil {
				reqLog.Error("failed to get daily usage", slog.String("error", err.Error()))
				used = 0
			}
			remaining := tierConfig.DailyPlanTokens - used
			if remaining < 0 {
				remaining = 0
			}
			percentage := (float64(used) / float64(tierConfig.DailyPlanTokens)) * 100
			response.DailyTokens = &TokenLimitInfo{
				Limit:      tierConfig.DailyPlanTokens,
				Used:       used,
				Remaining:  remaining,
				ResetsAt:   tierConfig.GetDailyResetTime(),
				UnderLimit: used < tierConfig.DailyPlanTokens,
				Percentage: percentage,
			}
		}

		// Deep research info
		dailyRunsUsed, _ := trackingService.GetUserDeepResearchRunsToday(ctx, userID)
		lifetimeRunsUsed, _ := trackingService.GetUserDeepResearchRunsLifetime(ctx, userID)
		response.DeepResearch = &DeepResearchInfo{
			DailyRuns:             tierConfig.DeepResearchDailyRuns,
			LifetimeRuns:          tierConfig.DeepResearchLifetimeRuns,
			TokenCap:              tierConfig.DeepResearchTokenCap,
			MaxActiveSessions:     tierConfig.DeepResearchMaxActiveSessions,
			DailyRunsUsed:         int(dailyRunsUsed),
			LifetimeRunsUsed:      int(lifetimeRunsUsed),
		}

		c.JSON(http.StatusOK, response)
	}
}

// MetricsHandler exposes request tracking metrics for monitoring.
func MetricsHandler(trackingService *Service, log *logger.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		// Only authenticated users can view metrics
		userID, exists := auth.GetUserID(c)
		if !exists {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "User not authenticated"})
			return
		}

		reqLog := log.WithContext(c.Request.Context()).WithComponent("metrics")
		reqLog.Debug("fetching request tracking metrics", slog.String("user_id", userID))

		metrics := trackingService.GetMetrics()

		c.JSON(http.StatusOK, gin.H{
			"request_tracking": metrics,
		})
	}
}
