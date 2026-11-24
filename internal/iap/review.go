package iap

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/eternisai/enchanted-proxy/internal/logger"
	pgdb "github.com/eternisai/enchanted-proxy/internal/storage/pg/sqlc"
)

// The entitlement review is a one-time job: Apple Plus entitlements written
// before purchases were recorded are kept for accounts that existed by the
// cutoff and reset to Free for the rest. Apple sold the lifetime plan on only
// a few days, so a later account can only have it from a purchase attached to
// another account. Anyone reset who holds a real purchase gets the plan back
// when the app next sends it.

// ReviewMode says whether the review runs and whether it changes anything.
type ReviewMode string

const (
	ReviewOff     ReviewMode = "off"
	ReviewDryRun  ReviewMode = "dry-run"
	ReviewEnforce ReviewMode = "enforce"
)

// ReviewConfig holds the review's settings.
type ReviewConfig struct {
	Mode ReviewMode
	// Cutoff is the first instant at which a new account no longer keeps its
	// unrecorded plan: the end of the configured day, UTC.
	Cutoff time.Time
	// Delay is how long after the server starts the review runs.
	Delay time.Duration
}

// defaultReviewCutoff is the day after the last day Apple recorded a sale of
// the lifetime plan, for time zones.
const defaultReviewCutoff = "2026-02-15"

// LoadReviewConfig reads ENTITLEMENT_REVIEW_MODE (off, dry-run or enforce;
// default and fallback off) and ENTITLEMENT_REVIEW_CUTOFF (a date; accounts
// created on or before it keep their plan).
func LoadReviewConfig() (ReviewConfig, error) {
	cfg := ReviewConfig{Mode: ReviewOff, Delay: 2 * time.Minute}
	switch mode := ReviewMode(strings.TrimSpace(os.Getenv("ENTITLEMENT_REVIEW_MODE"))); mode {
	case ReviewOff, ReviewDryRun, ReviewEnforce:
		cfg.Mode = mode
	case "":
	default:
		return cfg, fmt.Errorf("unknown ENTITLEMENT_REVIEW_MODE %q; the review stays off", mode)
	}
	day := strings.TrimSpace(os.Getenv("ENTITLEMENT_REVIEW_CUTOFF"))
	if day == "" {
		day = defaultReviewCutoff
	}
	cutoff, err := time.Parse(time.DateOnly, day)
	if err != nil {
		cfg.Mode = ReviewOff
		return cfg, fmt.Errorf("invalid ENTITLEMENT_REVIEW_CUTOFF %q; the review stays off", day)
	}
	cfg.Cutoff = cutoff.Add(24 * time.Hour)
	return cfg, nil
}

// Account is what the review needs to know about an entitlement's owner.
type Account struct {
	Exists  bool
	Created time.Time
}

// AccountLookup looks up accounts. An ID with no entry in the result could not
// be settled; an error means nothing is known about any of them.
type AccountLookup func(ctx context.Context, userIDs []string) (map[string]Account, error)

// Reviewer runs the entitlement review.
type Reviewer struct {
	db       *sql.DB
	accounts AccountLookup
	cfg      ReviewConfig
	logger   *logger.Logger
}

// NewReviewer returns the review job.
func NewReviewer(db *sql.DB, accounts AccountLookup, cfg ReviewConfig, log *logger.Logger) *Reviewer {
	return &Reviewer{db: db, accounts: accounts, cfg: cfg, logger: log}
}

// ReviewReport is what a run found and did. It is stored with the run.
type ReviewReport struct {
	Mode       ReviewMode `json:"mode"`
	Cutoff     time.Time  `json:"cutoff"`
	Candidates int        `json:"candidates"`
	Kept       int        `json:"kept"`
	// Accounts to reset, by reason. In enforce mode, Reset is how many rows
	// were set to Free.
	Deleted   []string `json:"deleted"`
	Later     []string `json:"created_after_cutoff"`
	Unsettled []string `json:"unsettled"`
	Reset     int      `json:"reset"`
	BackedUp  int64    `json:"backed_up"`
	Errors    []string `json:"errors,omitempty"`
}

func (rep *ReviewReport) fail(format string, args ...any) {
	rep.Errors = append(rep.Errors, fmt.Sprintf(format, args...))
}

// Start runs the review once, after its delay, unless ctx ends first.
func (r *Reviewer) Start(ctx context.Context) {
	if r.cfg.Mode == ReviewOff {
		return
	}
	go func() {
		select {
		case <-ctx.Done():
			return
		case <-time.After(r.cfg.Delay):
		}
		if _, err := r.Run(ctx); err != nil {
			r.logger.Error("entitlement review failed", slog.String("error", err.Error()))
		}
	}()
}

// Run reviews the entitlements once. It returns nil and does nothing when the
// review is off, when another server instance is running it, or, in enforce
// mode, when an enforced run has already finished.
func (r *Reviewer) Run(ctx context.Context) (*ReviewReport, error) {
	if r.cfg.Mode == ReviewOff {
		return nil, nil
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to begin entitlement review: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	q := pgdb.New(tx)

	locked, err := q.TryLockEntitlementReview(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to lock entitlement review: %w", err)
	}
	if !locked {
		r.logger.Info("entitlement review skipped: another instance is running it")
		return nil, nil
	}
	if r.cfg.Mode == ReviewEnforce {
		enforced, err := q.HasEnforcedEntitlementReview(ctx)
		if err != nil {
			return nil, fmt.Errorf("failed to read past entitlement reviews: %w", err)
		}
		if enforced {
			r.logger.Info("entitlement review skipped: it has already been enforced")
			return nil, nil
		}
	}
	runID, err := q.StartEntitlementReviewRun(ctx, string(r.cfg.Mode))
	if err != nil {
		return nil, fmt.Errorf("failed to record entitlement review: %w", err)
	}

	rep := r.review(ctx, q, runID)
	body, err := json.Marshal(rep)
	if err != nil {
		return nil, fmt.Errorf("failed to encode entitlement review report: %w", err)
	}
	report := string(body)
	if err := q.FinishEntitlementReviewRun(ctx, pgdb.FinishEntitlementReviewRunParams{ID: runID, Report: &report}); err != nil {
		return nil, fmt.Errorf("failed to record entitlement review report: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("failed to commit entitlement review: %w", err)
	}
	r.logger.Info("entitlement review finished",
		slog.String("mode", string(rep.Mode)),
		slog.Int("candidates", rep.Candidates),
		slog.Int("kept", rep.Kept),
		slog.Int("deleted", len(rep.Deleted)),
		slog.Int("created_after_cutoff", len(rep.Later)),
		slog.Int("unsettled", len(rep.Unsettled)),
		slog.Int("reset", rep.Reset),
		slog.Int("errors", len(rep.Errors)))
	return rep, nil
}

// review classifies the candidates and, in enforce mode, backs up the
// entitlements and resets the accounts to reset. Errors go into the report;
// with any error, nothing is reset.
func (r *Reviewer) review(ctx context.Context, q pgdb.Querier, runID int64) *ReviewReport {
	rep := &ReviewReport{Mode: r.cfg.Mode, Cutoff: r.cfg.Cutoff, Deleted: []string{}, Later: []string{}, Unsettled: []string{}}
	ids, err := q.ListUnrecordedApplePlusEntitlements(ctx)
	if err != nil {
		rep.fail("listing entitlements: %v", err)
		return rep
	}
	rep.Candidates = len(ids)
	if len(ids) == 0 {
		return rep
	}
	if r.accounts == nil {
		rep.fail("no account lookup configured")
		return rep
	}
	accounts, err := r.accounts(ctx, ids)
	if err != nil {
		rep.fail("looking up accounts: %v", err)
		return rep
	}
	for _, id := range ids {
		acc, ok := accounts[id]
		switch {
		case !ok:
			rep.Unsettled = append(rep.Unsettled, id)
		case !acc.Exists:
			rep.Deleted = append(rep.Deleted, id)
		case acc.Created.IsZero():
			rep.Unsettled = append(rep.Unsettled, id) // its age is unknown
		case !acc.Created.Before(r.cfg.Cutoff):
			rep.Later = append(rep.Later, id)
		default:
			rep.Kept++
		}
	}
	// Deleted accounts are expected to be a small share (8% when measured). A
	// lookup in the wrong Firebase project finds none of them, and one that
	// answers for few of them settles nothing. When most owners are missing or
	// unsettled, together, the run fails, so it does not count as the enforced
	// review.
	if unresolved := len(rep.Deleted) + len(rep.Unsettled); unresolved >= 5 && 2*unresolved > len(ids) {
		rep.fail("%d of %d accounts not found or could not be looked up; is the Firebase project right? Nothing reset", unresolved, len(ids))
		return rep
	}
	if r.cfg.Mode != ReviewEnforce {
		return rep
	}

	backedUp, err := q.BackupEntitlements(ctx, runID)
	if err != nil {
		rep.fail("backing up entitlements: %v", err)
		return rep
	}
	rep.BackedUp = backedUp
	reset, err := q.ResetUnrecordedApplePlusEntitlements(ctx, append(append([]string{}, rep.Deleted...), rep.Later...))
	if err != nil {
		rep.fail("resetting entitlements: %v", err)
		return rep
	}
	rep.Reset = len(reset)
	return rep
}
