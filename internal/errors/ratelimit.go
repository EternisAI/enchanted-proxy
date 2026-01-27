package errors

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
)

// RateLimitType represents the type of rate limit (soft allows fallback, hard blocks).
type RateLimitType string

const (
	RateLimitTypeSoft RateLimitType = "soft"
	RateLimitTypeHard RateLimitType = "hard"
)

// RateLimitError represents a standardized 429 Too Many Requests response.
// All rate limit responses from this proxy include rate_limit_type to distinguish
// from upstream provider 429s (which are passed through without this field).
type RateLimitError struct {
	Error         string        `json:"error"`
	Tier          string        `json:"tier"`
	RateLimitType RateLimitType `json:"rate_limit_type"`
	Limit         int64         `json:"limit"`
	Used          int64         `json:"used"`
	ResetsAt      time.Time     `json:"resets_at"`
}

// AbortWithRateLimit sends a 429 response with the RateLimitError and aborts the request.
func AbortWithRateLimit(c *gin.Context, err *RateLimitError) {
	c.AbortWithStatusJSON(http.StatusTooManyRequests, err)
}

// DailyLimitExceeded creates a RateLimitError for daily quota exhaustion.
func DailyLimitExceeded(tier, displayName string, limit, used int64, resetsAt time.Time, limitType RateLimitType) *RateLimitError {
	return &RateLimitError{
		Error:         displayName + " daily plan token limit exceeded",
		Tier:          tier,
		RateLimitType: limitType,
		Limit:         limit,
		Used:          used,
		ResetsAt:      resetsAt,
	}
}

// WeeklyLimitExceeded creates a RateLimitError for weekly quota exhaustion.
func WeeklyLimitExceeded(tier, displayName string, limit, used int64, resetsAt time.Time) *RateLimitError {
	return &RateLimitError{
		Error:         displayName + " weekly plan token limit exceeded",
		Tier:          tier,
		RateLimitType: RateLimitTypeHard,
		Limit:         limit,
		Used:          used,
		ResetsAt:      resetsAt,
	}
}

// MonthlyLimitExceeded creates a RateLimitError for monthly quota exhaustion.
func MonthlyLimitExceeded(tier, displayName string, limit, used int64, resetsAt time.Time) *RateLimitError {
	return &RateLimitError{
		Error:         displayName + " monthly plan token limit exceeded",
		Tier:          tier,
		RateLimitType: RateLimitTypeHard,
		Limit:         limit,
		Used:          used,
		ResetsAt:      resetsAt,
	}
}

// FallbackLimitExceeded creates a RateLimitError for fallback model quota exhaustion.
func FallbackLimitExceeded(tier, displayName string, limit, used int64, resetsAt time.Time) *RateLimitError {
	return &RateLimitError{
		Error:         displayName + " daily fallback limit exceeded",
		Tier:          tier,
		RateLimitType: RateLimitTypeHard,
		Limit:         limit,
		Used:          used,
		ResetsAt:      resetsAt,
	}
}
