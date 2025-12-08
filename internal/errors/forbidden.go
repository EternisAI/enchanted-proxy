package errors

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
)

// ForbiddenReason represents machine-readable reason codes for 403 errors.
type ForbiddenReason string

const (
	// Rate Limiting & Quotas
	ReasonModelNotAllowed   ForbiddenReason = "model_not_allowed"
	ReasonFeatureNotAllowed ForbiddenReason = "feature_not_allowed"

	// Deep Research
	ReasonActiveDeepResearchSession ForbiddenReason = "active_deep_research_session"
	ReasonDeepResearchDailyLimit    ForbiddenReason = "deep_research_daily_limit"
	ReasonDeepResearchLifetimeLimit ForbiddenReason = "deep_research_lifetime_limit"
	ReasonDeepResearchTokenCap      ForbiddenReason = "deep_research_token_cap"

	// Access Control
	ReasonChatNotOwned      ForbiddenReason = "chat_not_owned"
	ReasonSessionNotFound   ForbiddenReason = "session_not_found"
	ReasonInviteAlreadyUsed ForbiddenReason = "invite_already_used"
	ReasonInviteWrongUser   ForbiddenReason = "invite_wrong_user"

	// Subscription/Tier
	ReasonTierValidationFailed ForbiddenReason = "tier_validation_failed"
	ReasonSubscriptionExpired  ForbiddenReason = "subscription_expired"
)

// ForbiddenError represents a standardized 403 Forbidden response.
type ForbiddenError struct {
	Error     string                 `json:"error"`             // Technical error message (for logs)
	UIMessage string                 `json:"uiMessage"`         // User-friendly message (for UI display)
	Reason    ForbiddenReason        `json:"reason"`            // Machine-readable reason code
	Tier      string                 `json:"tier,omitempty"`    // User's current tier ("free", "pro", etc.)
	Details   map[string]interface{} `json:"details,omitempty"` // Optional context data
}

// NewForbiddenError creates a new ForbiddenError with the given parameters.
func NewForbiddenError(reason ForbiddenReason, errorMsg, uiMessage, tier string, details map[string]interface{}) *ForbiddenError {
	return &ForbiddenError{
		Error:     errorMsg,
		UIMessage: uiMessage,
		Reason:    reason,
		Tier:      tier,
		Details:   details,
	}
}

// AbortWithForbidden sends a 403 response with the ForbiddenError and aborts the request.
func AbortWithForbidden(c *gin.Context, err *ForbiddenError) {
	c.AbortWithStatusJSON(http.StatusForbidden, err)
}

// ModelNotAllowed creates a ForbiddenError for model access denial.
func ModelNotAllowed(model, tier, displayName string, allowedModels []string) *ForbiddenError {
	var errorMsg, uiMsg string
	if len(allowedModels) == 0 {
		errorMsg = "Model " + model + " not available for " + displayName + " tier"
		uiMsg = "This model is not available on your current plan."
	} else {
		errorMsg = "Model '" + model + "' not allowed. " + displayName + " tier allows only specific models."
		uiMsg = "This model is not available on your current plan. Upgrade to access all models."
	}

	return NewForbiddenError(
		ReasonModelNotAllowed,
		errorMsg,
		uiMsg,
		tier,
		map[string]interface{}{
			"requested_model": model,
			"allowed_models":  allowedModels,
		},
	)
}

// FeatureNotAllowed creates a ForbiddenError for feature access denial.
func FeatureNotAllowed(feature, tier, displayName, requiredTier string) *ForbiddenError {
	errorMsg := "Feature '" + feature + "' not available for " + displayName + " tier. Requires " + requiredTier + " tier."
	uiMsg := "This feature is not available on your current plan. Upgrade to unlock it."

	return NewForbiddenError(
		ReasonFeatureNotAllowed,
		errorMsg,
		uiMsg,
		tier,
		map[string]interface{}{
			"feature":       feature,
			"required_tier": requiredTier,
		},
	)
}

// ActiveDeepResearchSession creates a ForbiddenError for active session limit.
func ActiveDeepResearchSession(tier, displayName string, maxActive int) *ForbiddenError {
	errorMsg := "You have an active deep research session. Please complete or cancel it before starting a new one."
	uiMsg := "You already have an active deep research session. Please finish or cancel it first."

	return NewForbiddenError(
		ReasonActiveDeepResearchSession,
		errorMsg,
		uiMsg,
		tier,
		map[string]interface{}{
			"max_active_sessions": maxActive,
		},
	)
}

// DeepResearchDailyLimit creates a ForbiddenError for daily run limit.
func DeepResearchDailyLimit(tier, displayName string, used, limit int64, resetsAt time.Time) *ForbiddenError {
	errorMsg := "You've used all your deep research runs today. Resets at midnight UTC."
	uiMsg := "You've reached your daily limit. Your quota resets at midnight UTC."

	return NewForbiddenError(
		ReasonDeepResearchDailyLimit,
		errorMsg,
		uiMsg,
		tier,
		map[string]interface{}{
			"used":      used,
			"limit":     limit,
			"resets_at": resetsAt.Format(time.RFC3339),
		},
	)
}

// DeepResearchLifetimeLimit creates a ForbiddenError for lifetime run limit.
func DeepResearchLifetimeLimit(tier, displayName string, used, limit int64) *ForbiddenError {
	errorMsg := "You've used all your free deep research runs. Upgrade for more runs."
	uiMsg := "You've reached your lifetime limit of deep research runs. Upgrade to continue using this feature."

	return NewForbiddenError(
		ReasonDeepResearchLifetimeLimit,
		errorMsg,
		uiMsg,
		tier,
		map[string]interface{}{
			"used":  used,
			"limit": limit,
		},
	)
}

// DeepResearchTokenCap creates a ForbiddenError for per-run token cap.
func DeepResearchTokenCap(tier, displayName string, used, cap int) *ForbiddenError {
	errorMsg := "Deep research session exceeded token limit for " + displayName + " tier."
	uiMsg := "This deep research session has reached the token limit for your plan. Upgrade for higher limits."

	return NewForbiddenError(
		ReasonDeepResearchTokenCap,
		errorMsg,
		uiMsg,
		tier,
		map[string]interface{}{
			"used": used,
			"cap":  cap,
		},
	)
}

// ChatNotOwned creates a ForbiddenError for unauthorized chat access.
func ChatNotOwned(chatID string) *ForbiddenError {
	return NewForbiddenError(
		ReasonChatNotOwned,
		"Forbidden: You don't own this chat",
		"You don't have permission to access this chat.",
		"",
		map[string]interface{}{
			"chat_id": chatID,
		},
	)
}

// SessionNotFound creates a ForbiddenError for missing session.
func SessionNotFound(sessionType string) *ForbiddenError {
	return NewForbiddenError(
		ReasonSessionNotFound,
		"No active "+sessionType+" session found",
		"No active session found. Please start a new session.",
		"",
		map[string]interface{}{
			"session_type": sessionType,
		},
	)
}

// SessionNotOwned creates a ForbiddenError for unauthorized session access.
func SessionNotOwned(sessionID string) *ForbiddenError {
	return NewForbiddenError(
		ReasonChatNotOwned, // Reusing this reason code for session ownership
		"Forbidden: You don't own this session",
		"You don't have permission to access this session.",
		"",
		map[string]interface{}{
			"session_id": sessionID,
		},
	)
}

// InviteAlreadyUsed creates a ForbiddenError for already used invite codes.
func InviteAlreadyUsed() *ForbiddenError {
	return NewForbiddenError(
		ReasonInviteAlreadyUsed,
		"User already whitelisted",
		"You are already whitelisted and don't need an invite code.",
		"",
		nil,
	)
}

// InviteWrongUser creates a ForbiddenError for invite codes bound to different users.
func InviteWrongUser() *ForbiddenError {
	return NewForbiddenError(
		ReasonInviteWrongUser,
		"Code bound to a different user",
		"This invite code is bound to a different user and cannot be used.",
		"",
		nil,
	)
}

// TierValidationFailed creates a ForbiddenError for subscription validation failures.
func TierValidationFailed(errorDetail string) *ForbiddenError {
	return NewForbiddenError(
		ReasonTierValidationFailed,
		"Failed to validate subscription: "+errorDetail,
		"Unable to verify your subscription status. Please try again.",
		"",
		map[string]interface{}{
			"error": errorDetail,
		},
	)
}

// SubscriptionExpired creates a ForbiddenError for expired subscriptions.
func SubscriptionExpired(expiredAt time.Time) *ForbiddenError {
	return NewForbiddenError(
		ReasonSubscriptionExpired,
		"Subscription expired",
		"Your subscription has expired. Please renew to continue using premium features.",
		"",
		map[string]interface{}{
			"expired_at": expiredAt.Format(time.RFC3339),
		},
	)
}
