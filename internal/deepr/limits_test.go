package deepr

import (
	"context"
	"encoding/json"
	stderrors "errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eternisai/enchanted-proxy/internal/auth"
	"github.com/eternisai/enchanted-proxy/internal/config"
	"github.com/eternisai/enchanted-proxy/internal/errors"
	"github.com/eternisai/enchanted-proxy/internal/request_tracking"
	pgdb "github.com/eternisai/enchanted-proxy/internal/storage/pg/sqlc"
	"github.com/eternisai/enchanted-proxy/internal/tiers"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

// runCountQuerier answers the run-limit queries from fixed counts.
type runCountQuerier struct {
	pgdb.Querier
	active                             bool
	today, week, lifetime, serviceWide int64
	fail                               bool
	userIDs                            []string
	created                            int
	reapedBefore                       time.Time
	mu                                 sync.Mutex
}

func (q *runCountQuerier) err() error {
	if q.fail {
		return stderrors.New("database unavailable")
	}
	return nil
}

func (q *runCountQuerier) HasActiveDeepResearchRun(_ context.Context, ids []string) (bool, error) {
	q.userIDs = ids
	return q.active, q.err()
}
func (q *runCountQuerier) GetUserDeepResearchRunsToday(context.Context, []string) (int64, error) {
	return q.today, q.err()
}
func (q *runCountQuerier) GetUserDeepResearchRunsThisWeek(context.Context, []string) (int64, error) {
	return q.week, q.err()
}
func (q *runCountQuerier) GetUserDeepResearchRunsLifetime(context.Context, []string) (int64, error) {
	return q.lifetime, q.err()
}
func (q *runCountQuerier) CountDeepResearchRunsToday(context.Context) (int64, error) {
	return q.serviceWide, q.err()
}
func (q *runCountQuerier) CreateDeepResearchRun(context.Context, pgdb.CreateDeepResearchRunParams) (int64, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.created++
	return 1, nil
}
func (q *runCountQuerier) FailStaleDeepResearchRuns(_ context.Context, before time.Time) (int64, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.reapedBefore = before
	return 0, nil
}

type pairGroup struct{}

func (pairGroup) UserIDs(_ context.Context, userID string) ([]string, error) {
	return []string{userID, "other-account"}, nil
}

func limitService(q *runCountQuerier) *Service {
	return &Service{
		logger:                       testLogger(),
		queries:                      q,
		deepResearchRateLimitEnabled: true,
		cfg: &Config{
			Groups:           pairGroup{},
			MaxRunsPerDay:    25,
			FreeRunProviders: []string{auth.ProviderApple, auth.ProviderGoogle},
		},
	}
}

func identity(provider string) *auth.Identity {
	return &auth.Identity{UserID: "user-1", SignInProvider: provider}
}

func TestCheckRunAllowed(t *testing.T) {
	apple, google, anon, email := identity(auth.ProviderApple), identity(auth.ProviderGoogle), identity(auth.ProviderAnonymous), identity(auth.ProviderPassword)
	tests := []struct {
		name   string
		tier   tiers.Tier
		id     *auth.Identity
		q      *runCountQuerier
		reason errors.ForbiddenReason // "" = allowed
	}{
		{"free apple, first run", tiers.TierFree, apple, &runCountQuerier{}, ""},
		{"free google, first run", tiers.TierFree, google, &runCountQuerier{}, ""},
		{"free anonymous", tiers.TierFree, anon, &runCountQuerier{}, errors.ReasonDeepResearchSignIn},
		{"free email", tiers.TierFree, email, &runCountQuerier{}, errors.ReasonDeepResearchSignIn},
		{"free without identity", tiers.TierFree, nil, &runCountQuerier{}, errors.ReasonDeepResearchSignIn},
		{"free apple, run used", tiers.TierFree, apple, &runCountQuerier{lifetime: 1}, errors.ReasonDeepResearchLifetimeLimit},
		{"plus anonymous may run", tiers.TierPlus, anon, &runCountQuerier{}, ""},
		{"plus 5 today", tiers.TierPlus, anon, &runCountQuerier{today: 5, week: 5}, errors.ReasonDeepResearchDailyLimit},
		{"plus 10 this week", tiers.TierPlus, anon, &runCountQuerier{today: 1, week: 10}, errors.ReasonDeepResearchWeeklyLimit},
		{"plus active run", tiers.TierPlus, anon, &runCountQuerier{active: true}, errors.ReasonActiveDeepResearchSession},
		{"pro 10 today", tiers.TierPro, apple, &runCountQuerier{today: 10, week: 10}, errors.ReasonDeepResearchDailyLimit},
		{"pro 15 this week", tiers.TierPro, apple, &runCountQuerier{today: 2, week: 15}, errors.ReasonDeepResearchWeeklyLimit},
		{"pro 9 today, 14 this week", tiers.TierPro, apple, &runCountQuerier{today: 9, week: 14}, ""},
		{"pro active run", tiers.TierPro, apple, &runCountQuerier{active: true}, errors.ReasonActiveDeepResearchSession},
		{"service-wide ceiling", tiers.TierPro, apple, &runCountQuerier{serviceWide: 25}, errors.ReasonDeepResearchBusy},
		{"lookup failure refuses", tiers.TierPro, apple, &runCountQuerier{fail: true}, errors.ReasonTierValidationFailed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fe := limitService(tt.q).checkRunAllowed(context.Background(), "user-1", tt.id, tiers.Configs[tt.tier])
			if tt.reason == "" {
				if fe != nil {
					t.Fatalf("refused: %+v", fe)
				}
				return
			}
			if fe == nil || fe.Reason != tt.reason {
				t.Fatalf("got %+v, want reason %s", fe, tt.reason)
			}
			if fe.Error != fe.UIMessage || fe.Error == "" {
				t.Errorf("error %q is not the readable message %q", fe.Error, fe.UIMessage)
			}
		})
	}
}

func TestCheckRunAllowedCountsTheGroupAndCanBeDisabled(t *testing.T) {
	q := &runCountQuerier{}
	s := limitService(q)
	_ = s.checkRunAllowed(context.Background(), "user-1", identity(auth.ProviderApple), tiers.Configs[tiers.TierPro])
	if strings.Join(q.userIDs, ",") != "user-1,other-account" {
		t.Fatalf("counted %v, want the caller's group", q.userIDs)
	}

	s.deepResearchRateLimitEnabled = false
	if fe := s.checkRunAllowed(context.Background(), "user-1", identity(auth.ProviderAnonymous), tiers.Configs[tiers.TierFree]); fe != nil {
		t.Fatalf("limits disabled but refused: %+v", fe)
	}
}

func TestClientInputGating(t *testing.T) {
	s := &ActiveSession{}
	steps := []struct {
		event string // backend event before the client sends, "" for none
		want  bool
	}{
		{"", true},                      // the query
		{"", false},                     // nothing was asked
		{"research_progress", false},    // still nothing asked
		{"clarification_needed", true},  // first reply
		{"", false},                     // one reply per question
		{"clarification_needed", true},  // second reply
		{"clarification_needed", false}, // third round refused
	}
	for i, step := range steps {
		if step.event != "" {
			s.ObserveBackendEvent(step.event)
		}
		if got, _ := s.ClaimClientInput(); got != step.want {
			t.Fatalf("step %d: allowed = %v, want %v", i, got, step.want)
		}
	}

	errored := &ActiveSession{}
	errored.MarkQuerySent()
	errored.ObserveBackendEvent("clarification_needed")
	errored.ObserveBackendEvent("error")
	if ok, _ := errored.ClaimClientInput(); ok {
		t.Fatal("input forwarded after the run failed")
	}
}

func TestBackendHeaders(t *testing.T) {
	h := (&Config{RunMaxLLMCalls: 300, RunMaxTokens: 6_500_000, RunMaxSeconds: 12_600}).backendHeaders()
	want := map[string]string{
		"X-Run-Max-LLM-Calls": "300",
		"X-Run-Max-Tokens":    "6500000",
		"X-Run-Max-Seconds":   "12600",
	}
	for k, v := range want {
		if got := h.Get(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
	if len(h) != len(want) {
		t.Errorf("sent headers %v, want only the run budget", h)
	}
}

func TestLoadConfigDefaults(t *testing.T) {
	t.Setenv("DEEP_RESEARCH_FREE_RUN_PROVIDERS", "")
	c := LoadConfig(pairGroup{})
	if c.MaxRunsPerDay != 25 || c.RunMaxLLMCalls != 300 || c.RunMaxTokens != 6_500_000 || c.RunMaxSeconds != 12_600 {
		t.Errorf("defaults = %+v", c)
	}
	if strings.Join(c.FreeRunProviders, ",") != "apple.com,google.com" {
		t.Errorf("free run providers = %v", c.FreeRunProviders)
	}
	t.Setenv("DEEP_RESEARCH_FREE_RUN_PROVIDERS", "apple.com, google.com, password")
	if got := LoadConfig(nil).FreeRunProviders; strings.Join(got, ",") != "apple.com,google.com,password" {
		t.Errorf("configured providers = %v", got)
	}
}

func TestNextMonday(t *testing.T) {
	for in, want := range map[string]string{
		"2026-11-22T10:00:00Z": "2026-11-23", // Sunday
		"2026-11-23T00:00:00Z": "2026-11-30", // Monday
		"2026-11-26T23:59:00Z": "2026-11-30", // Thursday
	} {
		now, _ := time.Parse(time.RFC3339, in)
		if got := nextMonday(now).Format("2006-01-02"); got != want {
			t.Errorf("nextMonday(%s) = %s, want %s", in, got, want)
		}
	}
}

func TestRunReaperCutoff(t *testing.T) {
	q := &runCountQuerier{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { RunReaper(ctx, q, &Config{RunMaxSeconds: 3600}, testLogger()); close(done) }()
	deadline := time.Now().Add(2 * time.Second)
	for {
		q.mu.Lock()
		before := q.reapedBefore
		q.mu.Unlock()
		if !before.IsZero() {
			if age := time.Since(before); age < 89*time.Minute || age > 91*time.Minute {
				t.Errorf("reaped runs older than %v, want 1h30m (run length plus margin)", age)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("reaper did not run")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
}

// A free anonymous account opening the WebSocket gets a readable refusal and no run.
func TestWebSocketStartRefusesAnonymousFreeRun(t *testing.T) {
	gin.SetMode(gin.TestMode)
	config.AppConfig = &config.Config{RequestTrackingWorkerPoolSize: 1, RequestTrackingBufferSize: 1}
	log := testLogger()
	q := &runCountQuerier{}
	tq := &runQuerier{} // free tier, no history
	tracking := request_tracking.NewService(tq, log)
	t.Cleanup(func() { _ = tracking.Shutdown(context.Background()) })
	cfg := &Config{Groups: pairGroup{}, MaxRunsPerDay: 25, FreeRunProviders: []string{auth.ProviderApple, auth.ProviderGoogle}}

	engine := gin.New()
	engine.Use(func(c *gin.Context) {
		c.Set(string(auth.UserIDKey), "user-1")
		c.Request = c.Request.WithContext(auth.WithIdentity(c.Request.Context(), auth.Identity{UserID: "user-1", SignInProvider: auth.ProviderAnonymous}))
	})
	engine.GET("/ws", DeepResearchHandler(log, tracking, nil, nil, NewSessionManager(log), q, true, nil, cfg))
	srv := httptest.NewServer(engine)
	defer srv.Close()

	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http")+"/ws?chat_id=0d6f6b3e-8a54-4c1e-9a1f-2f4b6c7d8e9f", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, data, err := conn.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	var msg map[string]string
	_ = json.Unmarshal(data, &msg)
	if msg["error"] != "Sign in with Apple or Google to use your free deep research run." {
		t.Fatalf("message = %s", data)
	}
	if q.created != 0 {
		t.Fatalf("runs created = %d, want 0", q.created)
	}
}

// The REST clarify endpoint forwards only a reply to a question the backend asked.
func TestClarifyForwardsOnlyRepliesToQuestions(t *testing.T) {
	gin.SetMode(gin.TestMode)
	received := make(chan string, 4)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		for {
			_, m, err := c.ReadMessage()
			if err != nil {
				return
			}
			received <- string(m)
		}
	}))
	defer backend.Close()
	backendConn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(backend.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer backendConn.Close()

	log := testLogger()
	sm := NewSessionManager(log)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	const chat = "0d6f6b3e-8a54-4c1e-9a1f-2f4b6c7d8e9f"
	session := sm.CreateSession("test-user", chat, 1, backendConn, ctx, cancel)
	session.MarkQuerySent()

	engine := gin.New()
	engine.Use(withUser)
	engine.POST("/clarify", ClarifyDeepResearchHandler(log, nil, nil, nil, sm, untouchedQuerier{}, true, nil, &Config{}))
	post := func() int {
		req := httptest.NewRequest(http.MethodPost, "/clarify", strings.NewReader(`{"chat_id":"`+chat+`","response":"Europe"}`))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		engine.ServeHTTP(w, req)
		return w.Code
	}

	if code := post(); code != http.StatusConflict {
		t.Fatalf("reply without a question: status %d, want 409", code)
	}
	session.ObserveBackendEvent("clarification_needed")
	if code := post(); code != http.StatusOK {
		t.Fatalf("reply to a question: status %d, want 200", code)
	}
	select {
	case m := <-received:
		if !strings.Contains(m, "Europe") {
			t.Fatalf("backend received %q", m)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the reply did not reach the backend")
	}
	if code := post(); code != http.StatusConflict {
		t.Fatalf("second reply to one question: status %d, want 409", code)
	}
}
