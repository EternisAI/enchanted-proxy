package request_tracking

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/eternisai/enchanted-proxy/internal/auth"
	"github.com/eternisai/enchanted-proxy/internal/config"
	"github.com/eternisai/enchanted-proxy/internal/logger"
	"github.com/eternisai/enchanted-proxy/internal/routing"
	pgdb "github.com/eternisai/enchanted-proxy/internal/storage/pg/sqlc"
	"github.com/gin-gonic/gin"
)

// tierQuerier answers the queries the rate-limit path makes: a fixed tier and no tokens
// spent, so only the model gate can reject a request. Anything else is left to the
// embedded nil interface, which panics rather than quietly returning a zero value.
type tierQuerier struct {
	pgdb.Querier
	tier string
}

func (q tierQuerier) GetUserTier(_ context.Context, _ string) (pgdb.GetUserTierRow, error) {
	return pgdb.GetUserTierRow{SubscriptionTier: q.tier}, nil
}

func (q tierQuerier) GetUserPlanTokensThisMonth(_ context.Context, _ string) (int64, error) {
	return 0, nil
}

func (q tierQuerier) GetUserPlanTokensThisWeek(_ context.Context, _ string) (int64, error) {
	return 0, nil
}

func (q tierQuerier) GetUserPlanTokensToday(_ context.Context, _ string) (int64, error) {
	return 0, nil
}

func newTestRouter(t *testing.T, tier string) *gin.Engine {
	t.Helper()
	return newTestRouterWithKey(t, tier, "test-openai-key")
}

func newTestRouterWithKey(t *testing.T, tier, openAIKey string) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)

	t.Setenv("OPENAI_API_KEY", openAIKey)

	config.AppConfig = &config.Config{
		RateLimitEnabled:              true,
		RateLimitSoftMultiplier:       1.0,
		RequestTrackingWorkerPoolSize: 1,
		RequestTrackingBufferSize:     1,
	}

	log := logger.New(logger.Config{Level: slog.LevelError})

	configFile, err := os.Open("testdata/config.yaml")
	if err != nil {
		t.Fatalf("open test config: %v", err)
	}
	defer configFile.Close()

	routerConfig := &config.Config{}
	if err := config.LoadConfigFile(configFile, routerConfig); err != nil {
		t.Fatalf("load test config: %v", err)
	}

	service := NewService(tierQuerier{tier: tier}, log)
	t.Cleanup(func() { _ = service.Shutdown(context.Background()) })

	engine := gin.New()
	engine.Use(func(c *gin.Context) { c.Set(string(auth.UserIDKey), "test-user") })
	engine.Use(RequestTrackingMiddleware(service, log, routing.NewModelRouter(routerConfig, log)))
	engine.POST("/chat/completions", func(c *gin.Context) { c.Status(http.StatusOK) })

	return engine
}

func TestMiddlewareTierFloor(t *testing.T) {
	tests := []struct {
		name       string
		tier       string
		model      string
		wantStatus int
		// byFloor marks the cases the new tier floor is responsible for. Free is denied
		// by its own AllowedModels allowlist and never reaches the floor.
		byFloor bool
	}{
		{"plus denied a pro-only model", "plus", "gpt-5.5-pro", http.StatusForbidden, true},
		{"plus denied under the canonical name", "plus", "openai/gpt-5.5-pro", http.StatusForbidden, true},
		{"free denied by its allowlist", "free", "gpt-5.5-pro", http.StatusForbidden, false},
		{"pro allowed a pro-only model", "pro", "gpt-5.5-pro", http.StatusOK, false},
		{"plus allowed an ungated model", "plus", "gpt-4o", http.StatusOK, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			engine := newTestRouter(t, tc.tier)

			body := `{"model":"` + tc.model + `"}`
			req := httptest.NewRequest(http.MethodPost, "/chat/completions", strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			engine.ServeHTTP(rec, req)

			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body: %s)", rec.Code, tc.wantStatus, rec.Body.String())
			}

			if tc.byFloor && !strings.Contains(rec.Body.String(), "required_tier") {
				t.Errorf("403 did not come from the tier floor: %s", rec.Body.String())
			}
		})
	}
}

// Without an OpenAI key the model has no endpoints, so it drops out of the routing table
// and RouteModel would serve it through the OpenRouter wildcard. The gate still has to
// hold: an upstream outage is not a reason to open a Pro-only model to Plus.
func TestMiddlewareTierFloorWithoutEndpoints(t *testing.T) {
	engine := newTestRouterWithKey(t, "plus", "")

	req := httptest.NewRequest(http.MethodPost, "/chat/completions",
		strings.NewReader(`{"model":"gpt-5.5-pro"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d (body: %s)", rec.Code, http.StatusForbidden, rec.Body.String())
	}
}
