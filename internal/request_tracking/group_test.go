package request_tracking

import (
	"context"
	"encoding/json"
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

// usageQuerier reports each account's own usage, summed over the accounts a
// query asks for, and the same plan for everyone.
type usageQuerier struct {
	pgdb.Querier
	tier   string
	tokens map[string]int64 // plan tokens today, per account
	runs   map[string]int64 // deep research runs, per account
}

func (q usageQuerier) sum(m map[string]int64, ids []string) int64 {
	var total int64
	for _, id := range ids {
		total += m[id]
	}
	return total
}

func (q usageQuerier) GetUserTier(context.Context, string) (pgdb.GetUserTierRow, error) {
	return pgdb.GetUserTierRow{SubscriptionTier: q.tier}, nil
}

func (q usageQuerier) GetEntitlement(context.Context, string) (pgdb.GetEntitlementRow, error) {
	return pgdb.GetEntitlementRow{}, nil
}

func (q usageQuerier) GetUserPlanTokensThisMonth(_ context.Context, ids []string) (int64, error) {
	return q.sum(q.tokens, ids), nil
}

func (q usageQuerier) GetUserPlanTokensThisWeek(_ context.Context, ids []string) (int64, error) {
	return q.sum(q.tokens, ids), nil
}

func (q usageQuerier) GetUserPlanTokensToday(_ context.Context, ids []string) (int64, error) {
	return q.sum(q.tokens, ids), nil
}

func (q usageQuerier) GetUserDeepResearchRunsToday(_ context.Context, ids []string) (int64, error) {
	return q.sum(q.runs, ids), nil
}

func (q usageQuerier) GetUserDeepResearchRunsThisWeek(_ context.Context, ids []string) (int64, error) {
	return q.sum(q.runs, ids), nil
}

func (q usageQuerier) GetUserDeepResearchRunsLifetime(_ context.Context, ids []string) (int64, error) {
	return q.sum(q.runs, ids), nil
}

type fixedGroups map[string][]string

func (g fixedGroups) UserIDs(_ context.Context, userID string) ([]string, error) {
	if ids, ok := g[userID]; ok {
		return ids, nil
	}
	return []string{userID}, nil
}

type failingGroups struct{}

func (failingGroups) UserIDs(context.Context, string) ([]string, error) {
	return nil, errors.New("database unavailable")
}

func serveAs(t *testing.T, service *Service, userID string, failClosed bool, handler gin.HandlerFunc) *httptest.ResponseRecorder {
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
	engine := gin.New()
	engine.Use(func(c *gin.Context) { c.Set(string(auth.UserIDKey), userID) })
	if handler == nil {
		engine.Use(RequestTrackingMiddleware(service, log, nil))
		engine.POST("/chat/completions", func(c *gin.Context) { c.Status(http.StatusOK) })
		req := httptest.NewRequest(http.MethodPost, "/chat/completions", strings.NewReader(`{}`))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		engine.ServeHTTP(rec, req)
		return rec
	}
	engine.GET("/status", handler)
	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/status", nil))
	return rec
}

func newGroupService(t *testing.T, q pgdb.Querier, groups GroupLookup) *Service {
	t.Helper()
	config.AppConfig = &config.Config{RequestTrackingWorkerPoolSize: 1, RequestTrackingBufferSize: 1}
	service := NewService(q, groups, logger.New(logger.Config{Level: slog.LevelError + 4}))
	t.Cleanup(func() { _ = service.Shutdown(context.Background()) })
	return service
}

// Accounts that share a purchase share one day's usage: what one member spent
// counts against every member, and not against accounts outside the group.
func TestChatUsageCountsOverTheQuotaGroup(t *testing.T) {
	plus := tiers.Configs[tiers.TierPlus]
	q := usageQuerier{tier: "plus", tokens: map[string]int64{"b": plus.DailyPlanTokens}}
	service := newGroupService(t, q, fixedGroups{"a": {"a", "b"}, "b": {"a", "b"}})

	if rec := serveAs(t, service, "a", true, nil); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("a, whose group member used the day's tokens: status %d, want 429 (%s)", rec.Code, rec.Body)
	}
	if rec := serveAs(t, service, "c", true, nil); rec.Code != http.StatusOK {
		t.Fatalf("c, outside the group: status %d, want 200 (%s)", rec.Code, rec.Body)
	}
}

func TestAFailedGroupLookupHonoursFailClosed(t *testing.T) {
	service := newGroupService(t, usageQuerier{tier: "plus"}, failingGroups{})
	if rec := serveAs(t, service, "a", true, nil); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("fail closed: status %d, want 503", rec.Code)
	}
	if rec := serveAs(t, service, "a", false, nil); rec.Code != http.StatusOK {
		t.Fatalf("fail open: status %d, want 200", rec.Code)
	}
}

// The status a client shows must count deep research runs as admission does,
// over the quota group.
func TestStatusCountsUsageOverTheQuotaGroup(t *testing.T) {
	q := usageQuerier{tier: "plus", tokens: map[string]int64{"a": 100, "b": 200}, runs: map[string]int64{"a": 1, "b": 2}}
	service := newGroupService(t, q, fixedGroups{"a": {"a", "b"}})
	log := logger.New(logger.Config{Level: slog.LevelError + 4})

	rec := serveAs(t, service, "a", true, RateLimitStatusHandler(service, log))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	var got RateLimitStatusResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.DailyTokens == nil || got.DailyTokens.Used != 300 {
		t.Errorf("daily tokens = %+v, want 300 used", got.DailyTokens)
	}
	if got.DeepResearch == nil || got.DeepResearch.DailyRunsUsed != 3 || got.DeepResearch.WeeklyRunsUsed != 3 || got.DeepResearch.LifetimeRunsUsed != 3 {
		t.Errorf("deep research = %+v, want 3 runs used in each period", got.DeepResearch)
	}
}
