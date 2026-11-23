package deepr

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/eternisai/enchanted-proxy/internal/auth"
	"github.com/eternisai/enchanted-proxy/internal/errors"
	"github.com/eternisai/enchanted-proxy/internal/logger"
	pgdb "github.com/eternisai/enchanted-proxy/internal/storage/pg/sqlc"
	"github.com/eternisai/enchanted-proxy/internal/tiers"
)

// GroupLookup returns the accounts whose runs count toward the caller's limits.
type GroupLookup interface {
	UserIDs(ctx context.Context, userID string) ([]string, error)
}

// Admission runs fn in a database transaction that holds the deep research
// admission lock, so checking the limits and inserting a run cannot interleave
// with another start, on any server instance.
type Admission func(ctx context.Context, fn func(q pgdb.Querier) error) error

// LockedAdmission is the Admission backed by a Postgres advisory lock.
func LockedAdmission(db *sql.DB) Admission {
	return func(ctx context.Context, fn func(q pgdb.Querier) error) error {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("failed to begin deep research admission: %w", err)
		}
		defer func() { _ = tx.Rollback() }()

		q := pgdb.New(tx)
		if err := q.LockDeepResearchAdmission(ctx); err != nil {
			return fmt.Errorf("failed to lock deep research admission: %w", err)
		}
		if err := fn(q); err != nil {
			return err
		}
		return tx.Commit()
	}
}

// Config holds the deep research limits and backend settings, read once at startup.
type Config struct {
	Groups GroupLookup
	// Admission serializes starts; without it, limited starts are refused.
	Admission Admission

	// MaxRunsPerDay is the service-wide ceiling on runs per UTC day, counted as
	// the allowances count them: a run that failed within 60 seconds of starting
	// does not count. 0 disables it.
	MaxRunsPerDay int
	// FreeRunProviders are the sign-in providers whose accounts may use the free run.
	FreeRunProviders []string

	// Per-run budget sent to the backend, which clamps it to its own maxima.
	RunMaxLLMCalls int
	RunMaxTokens   int
	RunMaxSeconds  int
}

// Headers the backend reads on the WebSocket handshake.
const (
	headerRunMaxCalls   = "X-Run-Max-LLM-Calls"
	headerRunMaxTokens  = "X-Run-Max-Tokens"
	headerRunMaxSeconds = "X-Run-Max-Seconds"
)

// staleRunMargin is how long past the longest possible run an active run is
// left before the reaper marks it failed.
const staleRunMargin = 30 * time.Minute

// reapTimeout bounds one reaper sweep, so a sweep blocked on a lock does not
// hold up the ones after it.
const reapTimeout = time.Minute

// LoadConfig reads the settings from the environment, with defaults that sit just
// above the largest measured normal run.
func LoadConfig(groups GroupLookup) *Config {
	providers := []string{auth.ProviderApple, auth.ProviderGoogle}
	if raw := strings.TrimSpace(os.Getenv("DEEP_RESEARCH_FREE_RUN_PROVIDERS")); raw != "" {
		providers = nil
		for _, p := range strings.Split(raw, ",") {
			if p = strings.TrimSpace(p); p != "" {
				providers = append(providers, p)
			}
		}
	}
	return &Config{
		Groups:           groups,
		MaxRunsPerDay:    envInt("DEEP_RESEARCH_MAX_RUNS_PER_DAY", 25),
		FreeRunProviders: providers,
		RunMaxLLMCalls:   envInt("DEEP_RESEARCH_RUN_MAX_LLM_CALLS", 300),
		RunMaxTokens:     envInt("DEEP_RESEARCH_RUN_MAX_TOKENS", 6_500_000),
		RunMaxSeconds:    envInt("DEEP_RESEARCH_RUN_MAX_SECONDS", 12_600),
	}
}

// envInt reads a whole number of at least 0; anything else gives def, so a
// negative ceiling cannot switch the ceiling off.
func envInt(name string, def int) int {
	if v, err := strconv.Atoi(strings.TrimSpace(os.Getenv(name))); err == nil && v >= 0 {
		return v
	}
	return def
}

// backendHeaders carry the run's budget to the backend on the WebSocket handshake.
func (c *Config) backendHeaders() http.Header {
	h := http.Header{}
	if c == nil {
		return h
	}
	h.Set(headerRunMaxCalls, strconv.Itoa(c.RunMaxLLMCalls))
	h.Set(headerRunMaxTokens, strconv.Itoa(c.RunMaxTokens))
	h.Set(headerRunMaxSeconds, strconv.Itoa(c.RunMaxSeconds))
	return h
}

// readable makes the user-facing sentence the error text too: the iOS app shows
// the error field of a refused deep research request.
func readable(fe *errors.ForbiddenError) *errors.ForbiddenError {
	if fe != nil && fe.UIMessage != "" {
		fe.Error = fe.UIMessage
	}
	return fe
}

// startRun checks the caller's limits and, if a new run is allowed, records it
// as active. Every path that starts a run calls it. The check and the insert
// run under the admission lock, so concurrent starts are admitted one at a
// time and cannot all pass the same check. It returns the run's ID, or a
// refusal, or an error when the run could not be recorded.
func (s *Service) startRun(ctx context.Context, userID, chatID string, identity *auth.Identity, tier tiers.Config) (int64, *errors.ForbiddenError, error) {
	create := func(q pgdb.Querier) (int64, error) {
		return q.CreateDeepResearchRun(ctx, pgdb.CreateDeepResearchRunParams{
			UserID: userID,
			ChatID: chatID,
			Tier:   &tier.Name,
		})
	}
	if !s.deepResearchRateLimitEnabled {
		runID, err := create(s.queries)
		return runID, nil, err
	}
	if s.cfg == nil || s.cfg.Admission == nil {
		s.logger.WithContext(ctx).WithComponent("deepr").Error("deep research admission is not configured")
		return 0, readable(errors.TierValidationFailed("deep research limits are not configured")), nil
	}

	var runID int64
	var refusal *errors.ForbiddenError
	err := s.cfg.Admission(ctx, func(q pgdb.Querier) error {
		if refusal = s.checkRunAllowed(ctx, q, userID, identity, tier); refusal != nil {
			return nil
		}
		var err error
		runID, err = create(q)
		return err
	})
	if refusal != nil {
		return 0, refusal, nil
	}
	if err != nil {
		return 0, nil, err
	}
	return runID, nil, nil
}

// checkRunAllowed decides whether the caller may start a new run, reading
// through q, which startRun gives it inside the admission lock. It fails
// closed: a lookup error refuses the run.
func (s *Service) checkRunAllowed(ctx context.Context, q pgdb.Querier, userID string, identity *auth.Identity, tier tiers.Config) *errors.ForbiddenError {
	log := s.logger.WithContext(ctx).WithComponent("deepr")

	if s.cfg == nil || s.cfg.Groups == nil {
		log.Error("deep research limits are not configured")
		return readable(errors.TierValidationFailed("deep research limits are not configured"))
	}

	refuse := func(fe *errors.ForbiddenError) *errors.ForbiddenError {
		log.Warn("deep research run refused", slog.String("reason", string(fe.Reason)), slog.String("tier", tier.Name))
		return readable(fe)
	}
	failed := func(what string, err error) *errors.ForbiddenError {
		log.Error("deep research limit check failed", slog.String("check", what), slog.String("error", err.Error()))
		return readable(errors.TierValidationFailed("failed to check " + what))
	}

	// The free run is for accounts signed in with a provider that identifies a
	// person (Apple, Google), not anonymous or unverified email accounts.
	if tier.DeepResearchLifetimeRuns > 0 && (identity == nil || !identity.SignedInWith(s.cfg.FreeRunProviders...)) {
		return refuse(errors.DeepResearchSignInRequired(tier.Name))
	}

	userIDs, err := s.cfg.Groups.UserIDs(ctx, userID)
	if err != nil {
		return failed("quota group", err)
	}

	active, err := q.HasActiveDeepResearchRun(ctx, userIDs)
	if err != nil {
		return failed("active runs", err)
	}
	if active {
		return refuse(errors.ActiveDeepResearchSession(tier.Name, tier.DisplayName, 1))
	}

	if tier.DeepResearchLifetimeRuns > 0 {
		used, err := q.GetUserDeepResearchRunsLifetime(ctx, userIDs)
		if err != nil {
			return failed("lifetime runs", err)
		}
		if int(used) >= tier.DeepResearchLifetimeRuns {
			return refuse(errors.DeepResearchLifetimeLimit(tier.Name, tier.DisplayName, used, int64(tier.DeepResearchLifetimeRuns)))
		}
	}

	now := time.Now().UTC()
	if tier.DeepResearchDailyRuns > 0 {
		used, err := q.GetUserDeepResearchRunsToday(ctx, userIDs)
		if err != nil {
			return failed("daily runs", err)
		}
		if int(used) >= tier.DeepResearchDailyRuns {
			nextMidnight := now.Truncate(24 * time.Hour).Add(24 * time.Hour)
			return refuse(errors.DeepResearchDailyLimit(tier.Name, tier.DisplayName, used, int64(tier.DeepResearchDailyRuns), nextMidnight))
		}
	}

	if tier.DeepResearchWeeklyRuns > 0 {
		used, err := q.GetUserDeepResearchRunsThisWeek(ctx, userIDs)
		if err != nil {
			return failed("weekly runs", err)
		}
		if int(used) >= tier.DeepResearchWeeklyRuns {
			return refuse(errors.DeepResearchWeeklyLimit(tier.Name, tier.DisplayName, used, int64(tier.DeepResearchWeeklyRuns), nextMonday(now)))
		}
	}

	if s.cfg.MaxRunsPerDay > 0 {
		started, err := q.CountDeepResearchRunsToday(ctx)
		if err != nil {
			return failed("service-wide runs", err)
		}
		if int(started) >= s.cfg.MaxRunsPerDay {
			return refuse(errors.DeepResearchBusy(tier.Name))
		}
	}

	return nil
}

// nextMonday is the next Monday 00:00 UTC after now.
func nextMonday(now time.Time) time.Time {
	day := now.UTC().Truncate(24 * time.Hour)
	days := (8 - int(day.Weekday())) % 7
	if days == 0 {
		days = 7
	}
	return day.AddDate(0, 0, days)
}

// RunReaper marks runs failed that are still active long after any run could have
// finished, so a lost connection (a restart, a crash) does not leave the account
// with an active run forever. It runs until ctx is cancelled.
func RunReaper(ctx context.Context, queries pgdb.Querier, cfg *Config, log *logger.Logger) {
	maxRun := 12_600 * time.Second
	if cfg != nil && cfg.RunMaxSeconds > 0 {
		maxRun = time.Duration(cfg.RunMaxSeconds) * time.Second
	}
	reap := func() {
		cutoff := time.Now().Add(-(maxRun + staleRunMargin))
		sweepCtx, cancel := context.WithTimeout(ctx, reapTimeout)
		defer cancel()
		n, err := queries.FailStaleDeepResearchRuns(sweepCtx, cutoff)
		if err != nil {
			log.Error("failed to reap stale deep research runs", slog.String("error", err.Error()))
			return
		}
		if n > 0 {
			log.Warn("marked stale deep research runs failed", slog.Int64("runs", n))
		}
	}
	reap()
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			reap()
		}
	}
}
