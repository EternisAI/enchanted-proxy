package deepr

import (
	"context"
	"database/sql"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/eternisai/enchanted-proxy/internal/auth"
	"github.com/eternisai/enchanted-proxy/internal/config"
	"github.com/eternisai/enchanted-proxy/internal/logger"
	"github.com/eternisai/enchanted-proxy/internal/request_tracking"
	pgdb "github.com/eternisai/enchanted-proxy/internal/storage/pg/sqlc"
	"github.com/gin-gonic/gin"
)

// untouchedQuerier panics on any query, proving a refused request never reached the database.
type untouchedQuerier struct{ pgdb.Querier }

// runQuerier records run bookkeeping for a free-tier user with no history.
type runQuerier struct {
	pgdb.Querier
	mu        sync.Mutex
	created   int
	completed map[int64]string
}

func (q *runQuerier) GetUserTier(context.Context, string) (pgdb.GetUserTierRow, error) {
	return pgdb.GetUserTierRow{}, sql.ErrNoRows
}

func (q *runQuerier) CreateDeepResearchRun(context.Context, pgdb.CreateDeepResearchRunParams) (int64, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.created++
	return 42, nil
}

func (q *runQuerier) CompleteDeepResearchRun(_ context.Context, arg pgdb.CompleteDeepResearchRunParams) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.completed == nil {
		q.completed = map[int64]string{}
	}
	q.completed[arg.ID] = arg.Status
	return nil
}

func testLogger() *logger.Logger {
	return logger.New(logger.Config{Level: slog.LevelError})
}

func withUser(c *gin.Context) {
	c.Set(string(auth.UserIDKey), "test-user")
}

func newRefusalRouter() *gin.Engine {
	gin.SetMode(gin.TestMode)
	log := testLogger()
	q := untouchedQuerier{}
	engine := gin.New()
	engine.Use(withUser)
	engine.POST("/start", StartDeepResearchHandler(log, nil, nil, nil, nil, q, true, nil, nil, nil, nil))
	engine.POST("/clarify", ClarifyDeepResearchHandler(log, nil, nil, nil, nil, q, true, nil, nil))
	engine.GET("/ws", DeepResearchHandler(log, nil, nil, nil, nil, q, true, nil, nil))
	return engine
}

func TestStartAndClarifyRefuseMalformedIDs(t *testing.T) {
	engine := newRefusalRouter()
	for _, path := range []string{"/start", "/clarify"} {
		for _, body := range []string{
			`{"query":"q","response":"r","chat_id":"../x"}`,
			`{"query":"q","response":"r","chat_id":"a/b"}`,
			`{"query":"q","response":"r","chat_id":"..%2f..%2fx"}`,
			`{"query":"q","response":"r","chat_id":"0d6f6b3e-8a54-4c1e-9a1f-2f4b6c7d8e9f","user_message_id":"../m"}`,
		} {
			req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			engine.ServeHTTP(w, req)
			if w.Code != http.StatusBadRequest {
				t.Errorf("%s %s: status = %d, want 400", path, body, w.Code)
			}
			if !strings.Contains(w.Body.String(), "Invalid") {
				t.Errorf("%s %s: body %s lacks a readable error", path, body, w.Body.String())
			}
		}
	}
}

func TestWebSocketRefusesMalformedChatID(t *testing.T) {
	engine := newRefusalRouter()
	for _, raw := range []string{"..%2f..%2fx", "a%2Fb", "..", "a.b"} {
		req := httptest.NewRequest(http.MethodGet, "/ws?chat_id="+raw, nil)
		w := httptest.NewRecorder()
		engine.ServeHTTP(w, req)
		if w.Code != http.StatusBadRequest {
			t.Errorf("chat_id=%s: status = %d, want 400", raw, w.Code)
		}
	}
}

func TestStartMarksRunFailedWhenBackendUnreachable(t *testing.T) {
	gin.SetMode(gin.TestMode)
	config.AppConfig = &config.Config{RequestTrackingWorkerPoolSize: 1, RequestTrackingBufferSize: 1}

	// A port that refuses connections.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	t.Setenv("DEEP_RESEARCH_WS", addr)

	log := testLogger()
	q := &runQuerier{}
	tracking := request_tracking.NewService(q, log)
	t.Cleanup(func() { _ = tracking.Shutdown(context.Background()) })

	engine := gin.New()
	engine.Use(withUser)
	engine.POST("/start", StartDeepResearchHandler(log, tracking, nil, nil, NewSessionManager(log), q, false, nil, nil, nil, &Config{}))

	req := httptest.NewRequest(http.MethodPost, "/start",
		strings.NewReader(`{"query":"q","chat_id":"0d6f6b3e-8a54-4c1e-9a1f-2f4b6c7d8e9f"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (body %s)", w.Code, w.Body.String())
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.created != 1 {
		t.Fatalf("runs created = %d, want 1", q.created)
	}
	if got := q.completed[42]; got != "failed" {
		t.Errorf("run 42 status = %q, want \"failed\"", got)
	}
}

func TestBackendSessionURLEscapesIDs(t *testing.T) {
	u := backendSessionURL("ws", "backend:3031", "user1", "a/b?c")
	got := u.String()
	want := "ws://backend:3031/deep_research/user1/a%2Fb%3Fc/"
	if got != want {
		t.Errorf("URL = %q, want %q", got, want)
	}
	parsed, err := url.Parse(got)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.RawQuery != "" {
		t.Errorf("chat ID leaked into the query string: %q", parsed.RawQuery)
	}

	plain := backendSessionURL("ws", "backend:3031", "user1", "0d6f6b3e-8a54-4c1e-9a1f-2f4b6c7d8e9f")
	if s := plain.String(); s != "ws://backend:3031/deep_research/user1/0d6f6b3e-8a54-4c1e-9a1f-2f4b6c7d8e9f/" {
		t.Errorf("URL for a UUID = %q", s)
	}
}
