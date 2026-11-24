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
	reapHasDeadline                    bool
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
func (q *runCountQuerier) FailStaleDeepResearchRuns(ctx context.Context, before time.Time) (int64, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.reapedBefore = before
	_, q.reapHasDeadline = ctx.Deadline()
	return 0, nil
}

type pairGroup struct{}

func (pairGroup) UserIDs(_ context.Context, userID string) ([]string, error) {
	return []string{userID, "other-account"}, nil
}

// directAdmission runs the admission on q without a lock.
func directAdmission(q pgdb.Querier) Admission {
	return func(_ context.Context, fn func(pgdb.Querier) error) error { return fn(q) }
}

// limitService reads and writes runs only through its admission: its own
// queries panic if used.
func limitService(q *runCountQuerier) *Service {
	return &Service{
		logger:                       testLogger(),
		queries:                      untouchedQuerier{},
		deepResearchRateLimitEnabled: true,
		cfg: &Config{
			Groups:           pairGroup{},
			Admission:        directAdmission(q),
			MaxRunsPerDay:    25,
			FreeRunProviders: []string{auth.ProviderApple, auth.ProviderGoogle},
		},
	}
}

func identity(provider string) *auth.Identity {
	return &auth.Identity{UserID: "user-1", SignInProvider: provider}
}

func TestStartRun(t *testing.T) {
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
			runID, fe, err := limitService(tt.q).startRun(context.Background(), "user-1", "chat-1", tt.id, tiers.Configs[tt.tier])
			if err != nil {
				t.Fatal(err)
			}
			if tt.reason == "" {
				if fe != nil {
					t.Fatalf("refused: %+v", fe)
				}
				if runID != 1 || tt.q.created != 1 {
					t.Fatalf("run %d, runs created %d; want run 1 created once", runID, tt.q.created)
				}
				return
			}
			if fe == nil || fe.Reason != tt.reason {
				t.Fatalf("got %+v, want reason %s", fe, tt.reason)
			}
			if tt.q.created != 0 {
				t.Fatalf("refused, but %d runs created", tt.q.created)
			}
			if fe.Error != fe.UIMessage || fe.Error == "" {
				t.Errorf("error %q is not the readable message %q", fe.Error, fe.UIMessage)
			}
		})
	}
}

func TestStartRunCountsTheGroupAndCanBeDisabled(t *testing.T) {
	q := &runCountQuerier{}
	s := limitService(q)
	_, _, _ = s.startRun(context.Background(), "user-1", "chat-1", identity(auth.ProviderApple), tiers.Configs[tiers.TierPro])
	if strings.Join(q.userIDs, ",") != "user-1,other-account" {
		t.Fatalf("counted %v, want the caller's group", q.userIDs)
	}

	// With limits off, the run is recorded directly, without admission.
	unlimited := &runCountQuerier{}
	s.queries = unlimited
	s.cfg.Admission = nil
	s.deepResearchRateLimitEnabled = false
	if _, fe, err := s.startRun(context.Background(), "user-1", "chat-1", identity(auth.ProviderAnonymous), tiers.Configs[tiers.TierFree]); fe != nil || err != nil {
		t.Fatalf("limits disabled but refused: %+v, %v", fe, err)
	}
	if unlimited.created != 1 {
		t.Fatalf("runs created = %d, want 1", unlimited.created)
	}
}

// Without an admission, a limited start is refused rather than run unserialized.
func TestStartRunWithoutAdmissionRefuses(t *testing.T) {
	q := &runCountQuerier{}
	s := limitService(q)
	s.cfg.Admission = nil
	_, fe, err := s.startRun(context.Background(), "user-1", "chat-1", identity(auth.ProviderApple), tiers.Configs[tiers.TierPro])
	if err != nil || fe == nil || fe.Reason != errors.ReasonTierValidationFailed {
		t.Fatalf("got %+v, %v; want a refusal", fe, err)
	}
	if q.created != 0 {
		t.Fatalf("runs created = %d, want 0", q.created)
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

	errored := &ActiveSession{querySent: true}
	errored.ObserveBackendEvent("clarification_needed")
	errored.ObserveBackendEvent("error")
	if ok, _ := errored.ClaimClientInput(); ok {
		t.Fatal("input forwarded after the run failed")
	}

	// A backend that refuses before the query (for example when it is busy)
	// ends the run: the query is not accepted afterwards.
	refused := &ActiveSession{}
	refused.ObserveBackendEvent("error")
	if ok, _ := refused.ClaimClientInput(); ok {
		t.Fatal("query forwarded after the backend ended the run")
	}
}

// A session started over REST is published with its query claimed, so a client
// that connects before the proxy has sent the query cannot send one instead.
func TestQueryFromProxyIsClaimedAtCreation(t *testing.T) {
	sm := NewSessionManager(testLogger())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	rest := sm.CreateSession("test-user", "chat-rest", 1, nil, ctx, cancel, queryFromProxy)
	if ok, _ := rest.ClaimClientInput(); ok {
		t.Fatal("a client message took the REST query's place")
	}
	ws := sm.CreateSession("test-user", "chat-ws", 2, nil, ctx, cancel, queryFromClient)
	if ok, _ := ws.ClaimClientInput(); !ok {
		t.Fatal("the WebSocket client could not send its query")
	}
}

// Writes through a session are serialized: the WebSocket library supports one
// writer at a time, and detects concurrent writes with a panic.
func TestSessionWritesAreSerialized(t *testing.T) {
	const writers = 50
	received := make(chan string, writers)
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
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(backend.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	session := &ActiveSession{BackendConn: conn}
	payload := strings.Repeat("x", 64<<10) // large enough for writes to overlap
	var wg sync.WaitGroup
	for range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := session.WriteToBackend(websocket.TextMessage, []byte(payload)); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	for i := range writers {
		select {
		case m := <-received:
			if m != payload {
				t.Fatalf("message %d arrived damaged (%d bytes)", i, len(m))
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%d of %d messages arrived", i, writers)
		}
	}

	if err := (&ActiveSession{}).WriteToBackend(websocket.TextMessage, nil); err == nil {
		t.Error("a write without a backend connection succeeded")
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
	for _, name := range []string{
		"DEEP_RESEARCH_FREE_RUN_PROVIDERS",
		"DEEP_RESEARCH_MAX_RUNS_PER_DAY",
		"DEEP_RESEARCH_RUN_MAX_LLM_CALLS",
		"DEEP_RESEARCH_RUN_MAX_TOKENS",
		"DEEP_RESEARCH_RUN_MAX_SECONDS",
	} {
		t.Setenv(name, "")
	}
	c := LoadConfig(pairGroup{})
	if c.MaxRunsPerDay != 25 || c.RunMaxLLMCalls != 500 || c.RunMaxTokens != 6_500_000 || c.RunMaxSeconds != 12_600 {
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

// A negative setting gives the default: a negative ceiling must not switch the
// ceiling off. 0 still does.
func TestLoadConfigIgnoresNegativeValues(t *testing.T) {
	t.Setenv("DEEP_RESEARCH_MAX_RUNS_PER_DAY", "-1")
	if got := LoadConfig(nil).MaxRunsPerDay; got != 25 {
		t.Errorf("ceiling from -1 = %d, want the default 25", got)
	}
	t.Setenv("DEEP_RESEARCH_MAX_RUNS_PER_DAY", "0")
	if got := LoadConfig(nil).MaxRunsPerDay; got != 0 {
		t.Errorf("ceiling from 0 = %d, want 0 (off)", got)
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
	t.Cleanup(cancel) // also stops the reaper if the test fails below
	done := make(chan struct{})
	go func() { RunReaper(ctx, q, &Config{RunMaxSeconds: 3600}, testLogger()); close(done) }()
	deadline := time.Now().Add(2 * time.Second)
	for {
		q.mu.Lock()
		before, hasDeadline := q.reapedBefore, q.reapHasDeadline
		q.mu.Unlock()
		if !before.IsZero() {
			if age := time.Since(before); age < 89*time.Minute || age > 91*time.Minute {
				t.Errorf("reaped runs older than %v, want 1h30m (run length plus margin)", age)
			}
			if !hasDeadline {
				t.Error("the sweep has no deadline")
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
	tracking := request_tracking.NewService(tq, nil, log)
	t.Cleanup(func() { _ = tracking.Shutdown(context.Background()) })
	cfg := &Config{Groups: pairGroup{}, Admission: directAdmission(q), MaxRunsPerDay: 25, FreeRunProviders: []string{auth.ProviderApple, auth.ProviderGoogle}}

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
	session := sm.CreateSession("test-user", chat, 1, backendConn, ctx, cancel, queryFromProxy)

	engine := gin.New()
	engine.Use(withUser)
	engine.POST("/clarify", ClarifyDeepResearchHandler(log, nil, nil, nil, sm, untouchedQuerier{}, true, nil, &Config{}))
	post := func(response string) int {
		req := httptest.NewRequest(http.MethodPost, "/clarify", strings.NewReader(`{"chat_id":"`+chat+`","response":"`+response+`"}`))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		engine.ServeHTTP(w, req)
		return w.Code
	}

	// Each attempt sends its own text, so a refused reply that reached the
	// backend anyway would show up.
	if code := post("too early"); code != http.StatusConflict {
		t.Fatalf("reply without a question: status %d, want 409", code)
	}
	session.ObserveBackendEvent("clarification_needed")
	if code := post("Europe"); code != http.StatusOK {
		t.Fatalf("reply to a question: status %d, want 200", code)
	}
	if code := post("one too many"); code != http.StatusConflict {
		t.Fatalf("second reply to one question: status %d, want 409", code)
	}
	select {
	case m := <-received:
		if !strings.Contains(m, `"Europe"`) {
			t.Fatalf("backend received %q, want the accepted reply", m)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the reply did not reach the backend")
	}
	select {
	case m := <-received:
		t.Fatalf("backend also received %q, want only the accepted reply", m)
	case <-time.After(200 * time.Millisecond):
	}
}
