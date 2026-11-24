package request_tracking

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/eternisai/enchanted-proxy/internal/auth"
	"github.com/eternisai/enchanted-proxy/internal/config"
	"github.com/eternisai/enchanted-proxy/internal/logger"
	pgdb "github.com/eternisai/enchanted-proxy/internal/storage/pg/sqlc"
	"github.com/eternisai/enchanted-proxy/internal/tiers"
	"github.com/gin-gonic/gin"
)

var errLookup = errors.New("database unavailable")

// failingQuerier returns a fixed tier and fails the usage queries named in failing.
// Usage queries that do not fail report the given amount spent.
type failingQuerier struct {
	pgdb.Querier
	tier    string
	tierErr bool
	failing map[string]bool
	spent   int64
}

func (q failingQuerier) result(name string) (int64, error) {
	if q.failing[name] {
		return 0, errLookup
	}
	return q.spent, nil
}

func (q failingQuerier) GetUserTier(context.Context, string) (pgdb.GetUserTierRow, error) {
	if q.tierErr {
		return pgdb.GetUserTierRow{}, errLookup
	}
	return pgdb.GetUserTierRow{SubscriptionTier: q.tier}, nil
}

func (q failingQuerier) GetUserPlanTokensThisMonth(context.Context, []string) (int64, error) {
	return q.result("monthly")
}

func (q failingQuerier) GetUserPlanTokensThisWeek(context.Context, []string) (int64, error) {
	return q.result("weekly")
}

func (q failingQuerier) GetUserPlanTokensToday(context.Context, []string) (int64, error) {
	return q.result("daily")
}

func (q failingQuerier) GetUserFallbackPlanTokensToday(context.Context, pgdb.GetUserFallbackPlanTokensTodayParams) (int64, error) {
	return q.result("fallback")
}

func serveWithQuerier(t *testing.T, q failingQuerier, failClosed bool, body string) int {
	t.Helper()
	gin.SetMode(gin.TestMode)
	config.AppConfig = &config.Config{
		RateLimitEnabled:              true,
		RateLimitFailClosed:           failClosed,
		RateLimitSoftMultiplier:       1.0,
		RequestTrackingWorkerPoolSize: 1,
		RequestTrackingBufferSize:     1,
	}
	log := logger.New(logger.Config{Level: slog.LevelError + 4})
	service := NewService(q, nil, log)
	t.Cleanup(func() { _ = service.Shutdown(context.Background()) })

	engine := gin.New()
	engine.Use(func(c *gin.Context) { c.Set(string(auth.UserIDKey), "test-user") })
	engine.Use(RequestTrackingMiddleware(service, log, nil))
	engine.POST("/chat/completions", func(c *gin.Context) { c.Status(http.StatusOK) })

	req := httptest.NewRequest(http.MethodPost, "/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)
	return w.Code
}

func TestUsageLookupErrorsHonourFailClosed(t *testing.T) {
	// No tier limits weekly usage today; give Plus a weekly limit for this test only.
	original := tiers.Configs[tiers.TierPlus]
	withWeekly := original
	withWeekly.WeeklyPlanTokens = 1_000_000
	tiers.Configs[tiers.TierPlus] = withWeekly
	t.Cleanup(func() { tiers.Configs[tiers.TierPlus] = original })

	fallbackModel := original.FallbackModel
	tests := []struct {
		name string
		q    failingQuerier
		body string
	}{
		{"tier lookup", failingQuerier{tierErr: true}, `{}`},
		{"monthly usage", failingQuerier{tier: "free", failing: map[string]bool{"monthly": true}}, `{}`},
		{"weekly usage", failingQuerier{tier: "plus", failing: map[string]bool{"weekly": true}}, `{}`},
		{"daily usage", failingQuerier{tier: "plus", failing: map[string]bool{"daily": true}}, `{}`},
		{"fallback usage", failingQuerier{tier: "plus", failing: map[string]bool{"fallback": true}, spent: original.DailyPlanTokens},
			`{"model":"` + fallbackModel + `"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := serveWithQuerier(t, tt.q, true, tt.body); got != http.StatusServiceUnavailable {
				t.Errorf("fail closed: status = %d, want 503", got)
			}
			if got := serveWithQuerier(t, tt.q, false, tt.body); got != http.StatusOK {
				t.Errorf("fail open: status = %d, want 200", got)
			}
		})
	}
}

func TestSuccessfulLookupsUnaffectedByFailClosed(t *testing.T) {
	q := failingQuerier{tier: "plus"}
	if got := serveWithQuerier(t, q, true, `{}`); got != http.StatusOK {
		t.Errorf("status = %d, want 200", got)
	}
}
