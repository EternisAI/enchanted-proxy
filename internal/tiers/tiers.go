package tiers

import (
	"fmt"
	"time"
)

// Tier represents a subscription tier.
type Tier string

const (
	TierFree Tier = "free"
	TierPro  Tier = "pro"
)

// Config defines the limits and features for a subscription tier.
//
// Reset Times (all at 00:00 UTC):
//   - Monthly: Resets on 1st of month
//   - Weekly:  Resets every Monday
//   - Daily:   Resets every day
//
// Multiple quota periods can be active simultaneously. For example, a tier can have
// both weekly (100k tokens) and daily (20k tokens) limits. In this case:
//   - Users get 20k tokens per day
//   - BUT cannot exceed 100k tokens per week total
//   - Each limit is enforced independently
type Config struct {
	// Identity
	Name        string `json:"name"`
	DisplayName string `json:"display_name"`

	// Plan token limits (set to 0 for unlimited)
	MonthlyPlanTokens int64 `json:"monthly_plan_tokens"` // Resets 00:00 UTC on 1st of month
	WeeklyPlanTokens  int64 `json:"weekly_plan_tokens"`  // Resets 00:00 UTC every Monday
	DailyPlanTokens   int64 `json:"daily_plan_tokens"`   // Resets 00:00 UTC daily

	// Model access (allowlist only - empty array means all models allowed)
	AllowedModels []string `json:"allowed_models"` // Models allowed for this tier (empty = all allowed)

	// Deep research limits
	DeepResearchDailyRuns         int `json:"deep_research_daily_runs"`          // -1 = unlimited
	DeepResearchLifetimeRuns      int `json:"deep_research_lifetime_runs"`       // -1 = unlimited, 0 = check daily only
	DeepResearchTokenCap          int `json:"deep_research_token_cap"`           // Per-run token cap (GLM-4.6 tokens)
	DeepResearchMaxActiveSessions int `json:"deep_research_max_active_sessions"` // Max concurrent deep research jobs

	// Allowed features (features available for this tier, empty = all allowed)
	AllowedFeatures []Feature `json:"allowed_features"` // Features allowed for this tier (empty = all allowed)
}

// Feature represents a feature that can be allowed per tier.
type Feature string

const (
	FeatureDocumentUpload Feature = "document_upload"
	// Add more features here as needed:
	// FeatureAPIAccess      Feature = "api_access"
	// FeaturePrioritySupport Feature = "priority_support"
	// FeatureDeepResearch   Feature = "deep_research"
)

// Configs maps tier names to their configurations.
// Adding a new tier is as simple as adding an entry to this map!
var Configs = map[Tier]Config{
	TierFree: {
		Name:              "free",
		DisplayName:       "Free",
		MonthlyPlanTokens: 20_000,
		WeeklyPlanTokens:  0, // No weekly limit
		DailyPlanTokens:   0, // No daily limit
		// Note: AllowedModels must match exactly what clients send in the "model" field
		// Includes all known aliases to prevent blocking users
		// Free tier can only use: DeepSeek R1, Llama 3.3 70B, GLM-4.6, Dolphin Mistral (uncensored)
		AllowedModels: []string{
			"deepseek-r1-0528", "deepseek-r1", // DeepSeek R1 (1×)
			"llama3-3-70b", "llama-3.3-70b", // Llama 3.3 70B (1×)
			"zai-org/GLM-4.6", "glm-4.6", // GLM-4.6 (3×)
			"dolphin-mistral-eternis", "dolphin-mistral", // Dolphin Mistral (0.5×, uncensored)
		},
		DeepResearchDailyRuns:         0, // Not available daily
		DeepResearchLifetimeRuns:      1, // 1 lifetime run
		DeepResearchTokenCap:          8_000,
		DeepResearchMaxActiveSessions: 1,
		// Free tier does NOT have document upload feature
		AllowedFeatures: []Feature{}, // No special features
	},
	TierPro: {
		Name:                          "pro",
		DisplayName:                   "Pro",
		MonthlyPlanTokens:             0, // No monthly limit
		WeeklyPlanTokens:              0, // No weekly limit
		DailyPlanTokens:               500_000,
		AllowedModels:                 []string{}, // Empty = all models allowed
		DeepResearchDailyRuns:         10,
		DeepResearchLifetimeRuns:      0, // Check daily only
		DeepResearchTokenCap:          10_000,
		DeepResearchMaxActiveSessions: 0, // 0 = unlimited concurrent sessions
		// Pro tier has all features including document upload
		AllowedFeatures: []Feature{FeatureDocumentUpload},
	},
}

// Get returns the config for a tier.
func Get(tier Tier) (Config, error) {
	config, exists := Configs[tier]
	if !exists {
		return Config{}, fmt.Errorf("unknown tier: %s", tier)
	}
	return config, nil
}

// IsModelAllowed checks if a model is allowed for this tier.
// Empty AllowedModels means all models are allowed.
// Non-empty AllowedModels means only those specific models are allowed.
func (c Config) IsModelAllowed(modelID string) bool {
	// Empty list = all models allowed
	if len(c.AllowedModels) == 0 {
		return true
	}

	// Check if model is in the allowed list
	for _, allowed := range c.AllowedModels {
		if allowed == modelID {
			return true
		}
	}
	return false
}

// IsFeatureAllowed checks if a feature is allowed for this tier.
// Empty AllowedFeatures means all features are allowed.
// Non-empty AllowedFeatures means only those specific features are allowed.
func (c Config) IsFeatureAllowed(feature Feature) bool {
	// Empty list = all features allowed
	if len(c.AllowedFeatures) == 0 {
		return true
	}

	// Check if feature is in the allowed list
	for _, allowed := range c.AllowedFeatures {
		if allowed == feature {
			return true
		}
	}
	return false
}

// GetDailyResetTime returns when daily quota resets (00:00 UTC daily).
func (c Config) GetDailyResetTime() time.Time {
	if c.DailyPlanTokens == 0 {
		return time.Time{} // No daily quota
	}
	now := time.Now().UTC()
	tomorrow := now.AddDate(0, 0, 1)
	return time.Date(tomorrow.Year(), tomorrow.Month(), tomorrow.Day(), 0, 0, 0, 0, time.UTC)
}

// GetWeeklyResetTime returns when weekly quota resets (00:00 UTC every Monday).
func (c Config) GetWeeklyResetTime() time.Time {
	if c.WeeklyPlanTokens == 0 {
		return time.Time{} // No weekly quota
	}
	now := time.Now().UTC()

	// Calculate days until next Monday (simplified logic)
	daysUntilMonday := int((8 - int(now.Weekday())) % 7)
	if daysUntilMonday == 0 {
		daysUntilMonday = 7 // If today is Monday, reset is next Monday
	}

	nextMonday := now.AddDate(0, 0, daysUntilMonday)
	return time.Date(nextMonday.Year(), nextMonday.Month(), nextMonday.Day(), 0, 0, 0, 0, time.UTC)
}

// GetMonthlyResetTime returns when monthly quota resets (00:00 UTC on 1st of month).
func (c Config) GetMonthlyResetTime() time.Time {
	if c.MonthlyPlanTokens == 0 {
		return time.Time{} // No monthly quota
	}
	now := time.Now().UTC()
	nextMonth := now.AddDate(0, 1, 0)
	return time.Date(nextMonth.Year(), nextMonth.Month(), 1, 0, 0, 0, 0, time.UTC)
}
