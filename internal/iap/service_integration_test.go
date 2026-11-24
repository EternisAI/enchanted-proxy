//go:build integration

package iap

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/eternisai/enchanted-proxy/internal/logger"
	"github.com/eternisai/enchanted-proxy/internal/quota"
	"github.com/eternisai/enchanted-proxy/internal/storage/pg"
	pgdb "github.com/eternisai/enchanted-proxy/internal/storage/pg/sqlc"
	"github.com/google/uuid"
	_ "github.com/lib/pq"
	appstore "github.com/richzw/appstore"
)

func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	db, err := sql.Open("postgres", url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := pg.RunMigrations(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

// store holds the signed transactions of a test: what clients send, and what
// the App Store answers, both standing in for Apple's signatures.
type store struct {
	mu     sync.Mutex
	tokens map[string]*appstore.JWSTransaction // token -> transaction
	apple  map[string]string                   // transaction ID -> the App Store's token
	down   bool                                // the App Store cannot be reached
	calls  int
}

func (s *store) ParseTransaction(jws string) (*appstore.JWSTransaction, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, ok := s.tokens[jws]
	if !ok {
		return nil, errors.New("invalid signature")
	}
	c := *tx
	return &c, nil
}

func (s *store) SignedTransaction(_ context.Context, _ appstore.Environment, transactionID string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if s.down {
		return "", errors.New("connection refused")
	}
	token, ok := s.apple[transactionID]
	if !ok {
		return "", errNotAtApple
	}
	return token, nil
}

func (s *store) appleCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// purchase registers a purchase the App Store knows and returns the token a
// client sends for it.
func (s *store) purchase(tx *appstore.JWSTransaction) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	token := "client:" + tx.TransactionID
	s.tokens[token] = tx
	s.tokens["apple:"+tx.TransactionID] = tx
	s.apple[tx.TransactionID] = "apple:" + tx.TransactionID
	return token
}

type world struct {
	t      *testing.T
	db     *sql.DB
	q      *pgdb.Queries
	store  *store
	groups *quota.Groups
	s      *Service
}

func newWorld(t *testing.T) *world {
	db := openTestDB(t)
	st := &store{tokens: map[string]*appstore.JWSTransaction{}, apple: map[string]string{}}
	groups := quota.NewGroups(pgdb.New(db))
	s := &Service{
		db: db, queries: pgdb.New(db), parser: st, apple: st, policy: testPolicy(), groups: groups,
		logger: logger.New(logger.Config{Level: slog.LevelError + 4}), now: func() time.Time { return testNow },
	}
	return &world{t: t, db: db, q: pgdb.New(db), store: st, groups: groups, s: s}
}

func newID(name string) string { return name + "-" + uuid.NewString()[:8] }

// lifetime and monthly return a new purchase of each kind.
func (w *world) lifetime() (*appstore.JWSTransaction, string) {
	tx := lifetimeTx()
	tx.OriginalTransactionId = newID("plus")
	tx.TransactionID = tx.OriginalTransactionId
	return tx, w.store.purchase(tx)
}

func (w *world) monthly() (*appstore.JWSTransaction, string) {
	tx := subscriptionTx()
	tx.OriginalTransactionId = newID("pro")
	tx.TransactionID = tx.OriginalTransactionId + "-1"
	return tx, w.store.purchase(tx)
}

func (w *world) attach(userID, token string) *Attached {
	w.t.Helper()
	got, err := w.s.AttachAppStoreSubscription(context.Background(), userID, nil, token)
	if err != nil {
		w.t.Fatalf("attach for %s: %v", userID, err)
	}
	return got
}

// plan is the account's tier and the purchase its entitlement came from.
func (w *world) plan(userID string) (string, string) {
	w.t.Helper()
	ent, err := w.q.GetEntitlement(context.Background(), userID)
	if errors.Is(err, sql.ErrNoRows) {
		return "none", ""
	}
	if err != nil {
		w.t.Fatal(err)
	}
	from := ""
	if ent.AppleOriginalTransactionID != nil {
		from = *ent.AppleOriginalTransactionID
	}
	return ent.SubscriptionTier, from
}

func (w *world) expectPlan(userID, tier, from string) {
	w.t.Helper()
	if gotTier, gotFrom := w.plan(userID); gotTier != tier || (from != "" && gotFrom != from) {
		w.t.Fatalf("%s holds %s from %q, want %s from %q", userID, gotTier, gotFrom, tier, from)
	}
}

func (w *world) events(purchaseID string) []string {
	w.t.Helper()
	rows, err := w.db.Query(`SELECT kind, user_id, COALESCE(previous_user_id, '') FROM apple_purchase_events
		WHERE original_transaction_id = $1 ORDER BY id`, purchaseID)
	if err != nil {
		w.t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var kind, user, previous string
		if err := rows.Scan(&kind, &user, &previous); err != nil {
			w.t.Fatal(err)
		}
		out = append(out, fmt.Sprintf("%s %s %s", kind, user, previous))
	}
	return out
}

// A purchase attached to a second account moves there: the second account
// holds the plan, the first is Free, and both share one quota group. The App
// Store confirms it when first seen and when it moves, not on every launch.
func TestAPurchaseMovesToTheNewAccount(t *testing.T) {
	w := newWorld(t)
	a, b := newID("a"), newID("b")
	tx, token := w.lifetime()
	_, _ = w.groups.UserIDs(context.Background(), b) // b's group of one is cached

	if got := w.attach(a, token); got.Blocked || !got.ExpiresAt.Equal(lifetimeExpiry) {
		t.Fatalf("first attach %+v", got)
	}
	w.attach(a, token)
	if calls := w.store.appleCalls(); calls != 1 {
		t.Fatalf("App Store asked %d times for a purchase and a repeat by its holder, want 1", calls)
	}
	w.expectPlan(a, "plus", tx.OriginalTransactionId)

	w.attach(b, token)
	if calls := w.store.appleCalls(); calls != 2 {
		t.Fatalf("App Store asked %d times after the move, want 2", calls)
	}
	w.expectPlan(b, "plus", tx.OriginalTransactionId)
	w.expectPlan(a, "free", "")

	want := []string{a, b}
	slices.Sort(want)
	for _, id := range []string{a, b} {
		if got, err := w.groups.UserIDs(context.Background(), id); err != nil || !slices.Equal(got, want) {
			t.Fatalf("group of %s = %v, %v; want %v at once", id, got, err, want)
		}
	}
	if got := w.events(tx.OriginalTransactionId); !slices.Equal(got, []string{"attach " + a + " ", "move " + b + " " + a}) {
		t.Fatalf("events %q", got)
	}

	w.attach(a, token) // back again
	w.expectPlan(a, "plus", tx.OriginalTransactionId)
	w.expectPlan(b, "free", "")
}

// Nothing is written for a purchase the App Store could not confirm.
func TestAnUnconfirmedPurchaseWritesNothing(t *testing.T) {
	w := newWorld(t)
	a := newID("a")
	tx, token := w.lifetime()

	w.store.down = true
	if _, err := w.s.AttachAppStoreSubscription(context.Background(), a, nil, token); !errors.Is(err, ErrAppleUnavailable) {
		t.Fatalf("err %v, want ErrAppleUnavailable", err)
	}
	w.store.down = false
	delete(w.store.apple, tx.TransactionID)
	var refusal *Refusal
	if _, err := w.s.AttachAppStoreSubscription(context.Background(), a, nil, token); !errors.As(err, &refusal) || refusal.Reason != refusedUnknown {
		t.Fatalf("err %v, want a refusal for a purchase the App Store does not know", err)
	}
	if _, err := w.q.GetApplePurchase(context.Background(), tx.OriginalTransactionId); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("purchase recorded: %v", err)
	}
	w.expectPlan(a, "none", "")
}

// One purchase reaching a sixth account within 30 days is blocked: every
// account is Free, and later attaches grant nothing.
func TestTheSixthAccountBlocksAPurchase(t *testing.T) {
	w := newWorld(t)
	tx, token := w.lifetime()
	var accounts []string
	for i := range 6 {
		id := newID(fmt.Sprintf("acct%d", i))
		accounts = append(accounts, id)
		got := w.attach(id, token)
		if blocked := i == 5; got.Blocked != blocked {
			t.Fatalf("attach %d: blocked %v, want %v", i+1, got.Blocked, blocked)
		}
	}
	for _, id := range accounts {
		w.expectPlan(id, map[bool]string{true: "none", false: "free"}[id == accounts[5]], "")
	}
	if got := w.attach(accounts[0], token); !got.Blocked {
		t.Fatal("a blocked purchase granted a plan")
	}
	w.expectPlan(accounts[0], "free", "")
	row, err := w.q.GetApplePurchase(context.Background(), tx.OriginalTransactionId)
	if err != nil || !row.BlockedAt.Valid || row.BlockReason == nil || *row.BlockReason != "too_many_accounts" {
		t.Fatalf("purchase %+v, %v", row, err)
	}
}

// Moves back and forth between the two accounts of the routine case (the
// onboarding account and the signed-in one) never block.
func TestMovesBetweenTwoAccountsDoNotBlock(t *testing.T) {
	w := newWorld(t)
	a, b := newID("a"), newID("b")
	_, token := w.lifetime()
	for range 10 {
		if w.attach(a, token).Blocked || w.attach(b, token).Blocked {
			t.Fatal("blocked")
		}
	}
}

// A refunded or revoked purchase is blocked and its holder set to Free.
func TestARevokedPurchaseIsBlocked(t *testing.T) {
	w := newWorld(t)
	a := newID("a")
	tx, token := w.lifetime()
	w.attach(a, token)

	revoked := *tx
	revoked.RevocationDate = testNow.Add(-time.Hour).UnixMilli()
	w.store.tokens["revoked"] = &revoked
	var refusal *Refusal
	if _, err := w.s.AttachAppStoreSubscription(context.Background(), a, nil, "revoked"); !errors.As(err, &refusal) || refusal.Reason != refusedRevoked {
		t.Fatalf("err %v, want a revoked refusal", err)
	}
	w.expectPlan(a, "free", "")
	row, err := w.q.GetApplePurchase(context.Background(), tx.OriginalTransactionId)
	if err != nil || !row.BlockedAt.Valid || !row.RevokedAt.Valid {
		t.Fatalf("purchase %+v, %v; want blocked with its revocation time", row, err)
	}
	if got := w.attach(a, token); !got.Blocked {
		t.Fatal("the purchase granted a plan again after its revocation")
	}
}

// The app sends every purchase at each launch, in no fixed order: Pro wins
// whatever the order.
func TestPlusAndProInEitherOrderGivePro(t *testing.T) {
	w := newWorld(t)
	for _, proFirst := range []bool{true, false} {
		a := newID("a")
		_, plus := w.lifetime()
		pro, proToken := w.monthly()
		order := []string{plus, proToken}
		if proFirst {
			order = []string{proToken, plus}
		}
		for range 3 {
			for _, token := range order {
				w.attach(a, token)
			}
		}
		w.expectPlan(a, "pro", pro.OriginalTransactionId)
	}
}

// A plan from another provider is never lowered by a purchase, and survives
// the purchase moving away.
func TestAnotherProvidersPlanIsKept(t *testing.T) {
	w := newWorld(t)
	a, b := newID("a"), newID("b")
	_, token := w.lifetime()
	w.attach(a, token)
	if err := w.q.UpsertEntitlementWithTier(context.Background(), pgdb.UpsertEntitlementWithTierParams{
		UserID: a, SubscriptionTier: "pro", SubscriptionProvider: "stripe",
		SubscriptionExpiresAt: sql.NullTime{Time: testNow.Add(30 * 24 * time.Hour), Valid: true},
	}); err != nil {
		t.Fatal(err)
	}
	w.attach(a, token)
	w.expectPlan(a, "pro", "")
	w.attach(b, token)
	w.expectPlan(a, "pro", "")
	w.expectPlan(b, "plus", "")

	// As good a plan from another provider is kept too.
	c := newID("c")
	if err := w.q.UpsertEntitlementWithTier(context.Background(), pgdb.UpsertEntitlementWithTierParams{
		UserID: c, SubscriptionTier: "plus", SubscriptionProvider: "zcash",
		SubscriptionExpiresAt: sql.NullTime{Time: lifetimeExpiry, Valid: true},
	}); err != nil {
		t.Fatal(err)
	}
	_, other := w.lifetime()
	w.attach(c, other)
	if ent, err := w.q.GetEntitlement(context.Background(), c); err != nil || ent.SubscriptionProvider != "zcash" {
		t.Fatalf("entitlement %+v, %v; want the zcash plan kept", ent, err)
	}
}

// An Apple entitlement from before purchases were recorded gets its purchase
// recorded when the app sends it.
func TestAnUnrecordedApplePlanGetsItsPurchase(t *testing.T) {
	w := newWorld(t)
	a := newID("a")
	if _, err := w.db.Exec(`INSERT INTO entitlements (user_id, subscription_tier, subscription_expires_at, subscription_provider)
		VALUES ($1, 'plus', $2, 'apple')`, a, lifetimeExpiry); err != nil {
		t.Fatal(err)
	}
	tx, token := w.lifetime()
	w.attach(a, token)
	w.expectPlan(a, "plus", tx.OriginalTransactionId)
}

// staleReads answers the first purchase read with the caller as its holder,
// as a read made just before another account took the purchase would.
type staleReads struct {
	pgdb.Querier
	holder string
}

func (s staleReads) GetApplePurchase(ctx context.Context, id string) (pgdb.ApplePurchase, error) {
	row, err := s.Querier.GetApplePurchase(ctx, id)
	row.HolderUserID = s.holder
	return row, err
}

// When the purchase moves to another account between the first read and the
// lock, the attach starts again and confirms the move with the App Store.
func TestAMoveSeenOnlyUnderTheLockIsConfirmed(t *testing.T) {
	w := newWorld(t)
	a, b := newID("a"), newID("b")
	tx, token := w.lifetime()
	w.attach(b, token)
	calls := w.store.appleCalls()

	w.s.queries = staleReads{Querier: w.q, holder: a}
	w.attach(a, token)
	if got := w.store.appleCalls(); got != calls+1 {
		t.Fatalf("App Store asked %d more times, want 1", got-calls)
	}
	w.expectPlan(a, "plus", tx.OriginalTransactionId)
	w.expectPlan(b, "free", "")
}

// Parallel attaches of one purchase by many accounts are made one at a time:
// none fails, and the purchase ends blocked or with one account holding a plan
// from it.
func TestParallelAttachesStayConsistent(t *testing.T) {
	w := newWorld(t)
	tx, token := w.lifetime()
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := range 8 {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			if _, err := w.s.AttachAppStoreSubscription(context.Background(), id, nil, token); err != nil {
				errs <- err
			}
		}(newID(fmt.Sprintf("p%d", i)))
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("parallel attach failed: %v", err)
	}
	var holders int
	if err := w.db.QueryRow(`SELECT COUNT(*) FROM entitlements WHERE apple_original_transaction_id = $1 AND subscription_tier <> 'free'`,
		tx.OriginalTransactionId).Scan(&holders); err != nil {
		t.Fatal(err)
	}
	row, err := w.q.GetApplePurchase(context.Background(), tx.OriginalTransactionId)
	if err != nil {
		t.Fatal(err)
	}
	if !row.BlockedAt.Valid || holders != 0 {
		t.Fatalf("8 accounts in parallel: blocked %v, accounts with its plan %d; want blocked and none", row.BlockedAt.Valid, holders)
	}
	blocks := 0
	for _, e := range w.events(tx.OriginalTransactionId) {
		if e[:5] == "block" {
			blocks++
		}
	}
	if blocks != 1 {
		t.Fatalf("%d block events, want 1", blocks)
	}
}
