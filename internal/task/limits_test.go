package task

import (
	"context"
	"database/sql"
	"database/sql/driver"
	stderrors "errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/eternisai/enchanted-proxy/internal/auth"
	"github.com/eternisai/enchanted-proxy/internal/logger"
	pgdb "github.com/eternisai/enchanted-proxy/internal/storage/pg/sqlc"
	"github.com/eternisai/enchanted-proxy/internal/tiers"
	"github.com/gin-gonic/gin"
	"github.com/robfig/cron/v3"
)

var now = time.Date(2026, 11, 22, 10, 30, 0, 0, time.UTC)

func mustParse(t *testing.T, expr string) cron.Schedule {
	t.Helper()
	s, err := parseStrictSchedule(expr)
	if err != nil {
		t.Fatalf("parse %q: %v", expr, err)
	}
	return s
}

func TestParseStrictSchedule(t *testing.T) {
	for _, ok := range []string{"*/15 * * * *", "0 9 * * 1-5", "30 14 20 8 *", "0 9 * * MON", " 0 9 * * * "} {
		if _, err := parseStrictSchedule(ok); err != nil {
			t.Errorf("%q refused: %v", ok, err)
		}
	}
	for _, bad := range []string{"@every 10s", "@daily", "* * * * * *", "0 0 9 * * * 2026", "TZ=UTC 0 9 * * *", "CRON_TZ=UTC 0 9 * * *", "", "61 * * * *"} {
		_, err := parseStrictSchedule(bad)
		var v *ValidationError
		if !stderrors.As(err, &v) {
			t.Errorf("%q: err = %v, want a ValidationError", bad, err)
		}
	}
}

func TestMinInterval(t *testing.T) {
	cases := map[string]time.Duration{
		"* * * * *":       time.Minute,
		"*/5 * * * *":     5 * time.Minute,
		"0 * * * *":       time.Hour,
		"0 9 * * *":       24 * time.Hour,
		"0,59 0,23 * * *": time.Minute, // 23:59 then 00:00 the next day
		"0 0 1 1 *":       0,           // runs once within the next year
		"0 12 * * 1,3":    48 * time.Hour,
		"59 23 31 * *":    31 * 24 * time.Hour, // Dec 31 to Jan 31, the shortest step between 31sts
	}
	for expr, want := range cases {
		if got := minInterval(mustParse(t, expr), now); got != want {
			t.Errorf("%q: min interval %v, want %v", expr, got, want)
		}
	}
	// A burst far ahead still counts: every minute on one day of the year.
	if got := minInterval(mustParse(t, "* * 1 1 *"), now); got != time.Minute {
		t.Errorf("burst on Jan 1: min interval %v, want 1m", got)
	}
	// Consecutive firing days only at a month boundary far ahead: Jan 31 23:59, Feb 1 00:00.
	if got := minInterval(mustParse(t, "0,59 0,23 1,31 1,2 *"), now); got != time.Minute {
		t.Errorf("Jan 31 to Feb 1: min interval %v, want 1m", got)
	}
}

func TestScheduleChecksAreFast(t *testing.T) {
	start := time.Now()
	for _, expr := range []string{"* * * * *", "*/5 9-17 * * *", "0 * * * *"} {
		s := mustParse(t, expr)
		minInterval(s, now)
		exceedsRunsPerDay([]cron.Schedule{s, s, s, s, s}, now, 1_000_000)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("schedule checks took %v", elapsed)
	}
}

func TestExceedsRunsPerDay(t *testing.T) {
	hourly := mustParse(t, "0 * * * *")
	if exceedsRunsPerDay([]cron.Schedule{hourly}, now, 25) {
		t.Error("one hourly task (24 a day) exceeds 25")
	}
	if !exceedsRunsPerDay([]cron.Schedule{hourly, mustParse(t, "30 9 * * *"), mustParse(t, "45 9 * * *")}, now, 25) {
		t.Error("26 runs a day do not exceed 25")
	}
	// Two hourly tasks on the 1st of each month: 48 runs that day, however far ahead.
	firstOfMonth := mustParse(t, "0 * 1 * *")
	if !exceedsRunsPerDay([]cron.Schedule{firstOfMonth, mustParse(t, "30 * 1 * *")}, now, 25) {
		t.Error("48 runs on the 1st do not exceed 25")
	}
}

// limitQuerier answers the limit checks from canned rows and records writes.
type limitQuerier struct {
	pgdb.Querier
	created int64
	rows    []pgdb.ListOpenTasksForUsersRow
	userIDs []string

	inAdmission      bool     // set by the admission runner while fn runs
	writes           []string // in order; "@" marks a write made under the admission lock
	lockedFor        []string // the user IDs the admission locked
	deletedTasks     []string
	releasedCreation []string
}

func (q *limitQuerier) write(name string) {
	if q.inAdmission {
		name = "@" + name
	}
	q.writes = append(q.writes, name)
}

func (q *limitQuerier) CreateTask(_ context.Context, arg pgdb.CreateTaskParams) (pgdb.Task, error) {
	q.write("task")
	return pgdb.Task{TaskID: arg.TaskID, UserID: arg.UserID, ChatID: arg.ChatID, Type: arg.Type, Time: arg.Time, Status: arg.Status}, nil
}

func (q *limitQuerier) RecordTaskCreation(context.Context, pgdb.RecordTaskCreationParams) error {
	q.write("creation")
	return nil
}

func (q *limitQuerier) DeleteTask(_ context.Context, arg pgdb.DeleteTaskParams) (sql.Result, error) {
	q.deletedTasks = append(q.deletedTasks, arg.TaskID)
	return driver.RowsAffected(1), nil
}

func (q *limitQuerier) DeleteTaskCreation(_ context.Context, taskID string) error {
	q.releasedCreation = append(q.releasedCreation, taskID)
	return nil
}

func (q *limitQuerier) CountTaskCreationsSince(_ context.Context, arg pgdb.CountTaskCreationsSinceParams) (int64, error) {
	q.userIDs = arg.UserIds
	return q.created, nil
}

func (q *limitQuerier) ListOpenTasksForUsers(_ context.Context, userIDs []string) ([]pgdb.ListOpenTasksForUsersRow, error) {
	return q.rows, nil
}

type fixedTier struct{ tier tiers.Tier }

func (f fixedTier) GetUserTierConfig(context.Context, string) (tiers.Config, *time.Time, error) {
	return tiers.Configs[f.tier], nil, nil
}

type twoMemberGroup struct{}

func (twoMemberGroup) UserIDs(_ context.Context, userID string) ([]string, error) {
	return []string{userID, "other-account"}, nil
}

func newLimitService(tier tiers.Tier, q *limitQuerier) *Service {
	return &Service{
		logger:     logger.New(logger.Config{Level: slog.LevelError + 4}),
		queries:    q,
		tierLookup: fixedTier{tier: tier},
		groups:     twoMemberGroup{},
		admission: func(_ context.Context, userIDs []string, fn func(pgdb.Querier) error) error {
			q.lockedFor = userIDs
			q.inAdmission = true
			defer func() { q.inAdmission = false }()
			return fn(q)
		},
	}
}

// admitErr runs the admission of one task and returns only its error.
func (s *Service) admitErr(ctx context.Context, userID string, req *CreateTaskRequest, now time.Time) error {
	_, err := s.admitTask(ctx, userID, "task-1", req, now)
	return err
}

func recurringRows(exprs ...string) []pgdb.ListOpenTasksForUsersRow {
	var rows []pgdb.ListOpenTasksForUsersRow
	for _, e := range exprs {
		rows = append(rows, pgdb.ListOpenTasksForUsersRow{Type: "recurring", Time: e, Status: "active", CreatedAt: now.Add(-time.Hour)})
	}
	return rows
}

func request(kind, expr string) *CreateTaskRequest {
	return &CreateTaskRequest{ChatID: "chat-1", TaskName: "n", TaskText: "t", Type: kind, Time: expr}
}

func limitReason(err error) string {
	var l *LimitError
	if stderrors.As(err, &l) {
		return l.Reason
	}
	return ""
}

func TestCreationLimits(t *testing.T) {
	tenHourly := recurringRows("0 1 * * *", "0 2 * * *", "0 3 * * *", "0 4 * * *", "0 5 * * *",
		"0 6 * * *", "0 7 * * *", "0 8 * * *", "0 9 * * *", "0 10 * * *")
	past := pgdb.ListOpenTasksForUsersRow{Type: "one_time", Time: "0 9 1 1 *", Status: "active", CreatedAt: time.Date(2026, 12, 25, 0, 0, 0, 0, time.UTC).AddDate(-1, 0, 0)}
	pending := pgdb.ListOpenTasksForUsersRow{Type: "one_time", Time: "0 9 24 12 *", Status: "active", CreatedAt: now.Add(-time.Hour)}
	twentyPending := make([]pgdb.ListOpenTasksForUsersRow, 20)
	for i := range twentyPending {
		twentyPending[i] = pending
	}

	tests := []struct {
		name   string
		tier   tiers.Tier
		q      *limitQuerier
		req    *CreateTaskRequest
		reason string // "" means allowed
	}{
		{"free daily task", tiers.TierFree, &limitQuerier{}, request("recurring", "0 9 * * *"), ""},
		{"free hourly task", tiers.TierFree, &limitQuerier{}, request("recurring", "0 * * * *"), ""},
		{"free every minute", tiers.TierFree, &limitQuerier{}, request("recurring", "* * * * *"), "min_interval"},
		{"free every 30 minutes", tiers.TierFree, &limitQuerier{}, request("recurring", "*/30 * * * *"), "min_interval"},
		{"free second hourly task", tiers.TierFree, &limitQuerier{rows: recurringRows("0 * * * *")}, request("recurring", "30 * * * *"), "runs_per_day"},
		{"free eleventh recurring task", tiers.TierFree, &limitQuerier{rows: tenHourly}, request("recurring", "0 20 * * *"), "active_recurring_tasks"},
		{"free 41st task today", tiers.TierFree, &limitQuerier{created: 40}, request("recurring", "0 9 * * *"), "tasks_created_per_day"},
		{"free one-time with past ones", tiers.TierFree, &limitQuerier{rows: []pgdb.ListOpenTasksForUsersRow{past, past, past}}, request("one_time", "0 9 24 12 *"), ""},
		{"free 21st pending one-time", tiers.TierFree, &limitQuerier{rows: twentyPending}, request("one_time", "0 9 24 12 *"), "pending_one_time_tasks"},
		{"plus every 15 minutes", tiers.TierPlus, &limitQuerier{}, request("recurring", "*/15 * * * *"), ""},
		{"plus every 5 minutes", tiers.TierPlus, &limitQuerier{}, request("recurring", "*/5 * * * *"), "min_interval"},
		{"pro every 5 minutes in working hours", tiers.TierPro, &limitQuerier{}, request("recurring", "*/5 9-17 * * *"), ""},
		{"pro every 5 minutes all day", tiers.TierPro, &limitQuerier{}, request("recurring", "*/5 * * * *"), "runs_per_day"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := newLimitService(tt.tier, tt.q).admitErr(context.Background(), "user-1", tt.req, now)
			if got := limitReason(err); got != tt.reason {
				t.Fatalf("reason = %q (err %v), want %q", got, err, tt.reason)
			}
			if tt.reason == "" && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestCreationLimitsCountTheWholeGroup(t *testing.T) {
	q := &limitQuerier{}
	_ = newLimitService(tiers.TierFree, q).admitErr(context.Background(), "user-1", request("recurring", "0 9 * * *"), now)
	if strings.Join(q.userIDs, ",") != "user-1,other-account" {
		t.Fatalf("counted user IDs %v, want the caller's group", q.userIDs)
	}
}

func TestLimitMessagesNameThePlanAndLimit(t *testing.T) {
	err := newLimitService(tiers.TierFree, &limitQuerier{}).admitErr(context.Background(), "user-1", request("recurring", "* * * * *"), now)
	if err == nil || !strings.Contains(err.Error(), "Free plan") || !strings.Contains(err.Error(), "once an hour") {
		t.Fatalf("message %q lacks the plan or the limit", err)
	}
}

func TestTextBounds(t *testing.T) {
	s := newLimitService(tiers.TierFree, &limitQuerier{})
	long := request("recurring", "0 9 * * *")
	long.TaskName = strings.Repeat("é", 101)
	var v *ValidationError
	if err := s.admitErr(context.Background(), "user-1", long, now); !stderrors.As(err, &v) {
		t.Errorf("101-character name: err = %v, want ValidationError", err)
	}
	long = request("recurring", "0 9 * * *")
	long.TaskText = strings.Repeat("a", 2001)
	if err := s.admitErr(context.Background(), "user-1", long, now); !stderrors.As(err, &v) {
		t.Errorf("2001-character text: err = %v, want ValidationError", err)
	}
}

func TestCreateTaskHandlerStatusForRefusals(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		expr   string
		status int
		body   string
	}{
		{"* * * * *", http.StatusForbidden, `"reason":"task_limit"`},
		{"@every 10s", http.StatusBadRequest, "five-field cron"},
	} {
		s := newLimitService(tiers.TierFree, &limitQuerier{})
		engine := gin.New()
		engine.Use(func(c *gin.Context) { c.Set(string(auth.UserIDKey), "user-1") })
		engine.POST("/api/v1/tasks", NewHandler(s, s.logger).CreateTask)

		body := `{"chat_id":"chat-1","task_name":"n","task_text":"t","type":"recurring","time":"` + tc.expr + `"}`
		req := httptest.NewRequest(http.MethodPost, "/api/v1/tasks", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		engine.ServeHTTP(w, req)
		if w.Code != tc.status || !strings.Contains(w.Body.String(), tc.body) {
			t.Errorf("%q: status %d body %s, want %d containing %s", tc.expr, w.Code, w.Body.String(), tc.status, tc.body)
		}
	}
}
