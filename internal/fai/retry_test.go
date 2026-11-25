package fai

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eternisai/enchanted-proxy/internal/logger"
	pgdb "github.com/eternisai/enchanted-proxy/internal/storage/pg/sqlc"
	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

func testLog(index uint) types.Log {
	return types.Log{TxHash: common.HexToHash("0xabc"), Index: index}
}

func TestSettlementRetrySchedule(t *testing.T) {
	r := newSettlementRetries()
	start := time.Date(2026, 11, 22, 0, 0, 0, 0, time.UTC)
	l := testLog(1)
	transient := retryable(errors.New("database unavailable"))

	r.add(l, start)
	r.add(l, start.Add(30*time.Second)) // already queued: schedule unchanged
	if got := r.due(start.Add(59 * time.Second)); len(got) != 0 {
		t.Fatalf("due before the first delay: %d", len(got))
	}

	now := start.Add(time.Minute)
	wantDelays := []time.Duration{2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 16 * time.Minute, 32 * time.Minute, time.Hour, time.Hour}
	for i, want := range wantDelays {
		if got := r.due(now); len(got) != 1 {
			t.Fatalf("attempt %d: due = %d, want 1", i, len(got))
		}
		if gaveUp := r.result(l, transient, now); gaveUp {
			t.Fatalf("attempt %d: gave up early", i)
		}
		if got := r.due(now.Add(want - time.Second)); len(got) != 0 {
			t.Fatalf("attempt %d: due before its %v delay", i, want)
		}
		now = now.Add(want)
	}

	if gaveUp := r.result(l, transient, start.Add(settlementRetryGiveUpAfter)); !gaveUp {
		t.Fatal("did not give up after the give-up time")
	}
	if r.len() != 0 {
		t.Fatal("entry kept after giving up")
	}
}

func TestSettlementRetryDropsOnSuccessOrPermanentFailure(t *testing.T) {
	r := newSettlementRetries()
	now := time.Now()
	r.add(testLog(1), now)
	r.add(testLog(2), now)
	r.result(testLog(1), nil, now)
	r.result(testLog(2), errors.New("payment insufficient"), now)
	if r.len() != 0 {
		t.Fatalf("entries left: %d", r.len())
	}
}

// failingIntentQuerier fails the intent lookup, as an unavailable database would.
type failingIntentQuerier struct{ pgdb.Querier }

func (failingIntentQuerier) GetFaiPaymentIntentByPaymentID(context.Context, string) (pgdb.FaiPaymentIntent, error) {
	return pgdb.FaiPaymentIntent{}, errors.New("connection refused")
}

func TestHandlerClassifiesFailures(t *testing.T) {
	sig := common.HexToHash("0x01")
	s := NewService(nil, failingIntentQuerier{}, logger.New(logger.Config{Level: slog.LevelError + 4}))
	s.eventSignature = sig

	err := s.handlePaymentReceivedEvent(context.Background(), types.Log{Topics: []common.Hash{sig}, Data: make([]byte, 128)})
	if !isRetryable(err) {
		t.Errorf("database failure: err = %v, want retryable", err)
	}

	err = s.handlePaymentReceivedEvent(context.Background(), types.Log{Topics: []common.Hash{sig}, Data: make([]byte, 10)})
	if err == nil || isRetryable(err) {
		t.Errorf("malformed event: err = %v, want a permanent error", err)
	}
}

// fakeChain serves logs and block times; FilterLogs fails for the ranges listed in failFrom.
type fakeChain struct {
	mu            sync.Mutex
	failFrom      map[uint64]bool
	queried       []blockRange
	blockTimes    map[uint64]time.Time
	head          uint64
	calls         []string         // "subscribe" and "head", in order
	live          chan<- types.Log // the listener's subscription channel
	onFilter      func()           // runs at the start of each FilterLogs call
	subs          []*fakeSubscription
	subscribeErrs map[int]error // by subscribe call, from 0
}

type fakeSubscription struct {
	errs         chan error
	mu           sync.Mutex
	unsubscribed int
}

func (f *fakeSubscription) Unsubscribe() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.unsubscribed++
}

func (f *fakeSubscription) Err() <-chan error { return f.errs }

func (f *fakeChain) SubscribeFilterLogs(_ context.Context, _ ethereum.FilterQuery, ch chan<- types.Log) (ethereum.Subscription, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := len(f.subs)
	f.calls = append(f.calls, "subscribe")
	if err := f.subscribeErrs[n]; err != nil {
		f.subs = append(f.subs, nil)
		return nil, err // go-ethereum returns a nil subscription on failure
	}
	sub := &fakeSubscription{errs: make(chan error, 1)}
	f.subs = append(f.subs, sub)
	f.live = ch
	return sub, nil
}

func (f *fakeChain) BlockNumber(context.Context) (uint64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "head")
	return f.head, nil
}

func (f *fakeChain) FilterLogs(_ context.Context, q ethereum.FilterQuery) ([]types.Log, error) {
	if f.onFilter != nil {
		f.onFilter()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	r := blockRange{from: q.FromBlock.Uint64(), to: q.ToBlock.Uint64()}
	f.queried = append(f.queried, r)
	if f.failFrom[r.from] {
		delete(f.failFrom, r.from) // fail once
		return nil, errors.New("rpc unavailable")
	}
	return nil, nil
}

func (f *fakeChain) HeaderByNumber(_ context.Context, n *big.Int) (*types.Header, error) {
	t, ok := f.blockTimes[n.Uint64()]
	if !ok {
		return nil, errors.New("unknown block")
	}
	return &types.Header{Time: uint64(t.Unix())}, nil
}

func TestFailedReplayStaysPendingUntilItCompletes(t *testing.T) {
	chain := &fakeChain{failFrom: map[uint64]bool{1500: true}}
	s := newChainService(nil, chain)

	if err := s.replay(context.Background(), blockRange{from: 1000, to: 2999}); err == nil {
		t.Fatal("replay reported success although a batch failed")
	}
	if s.pendingReplay == nil || *s.pendingReplay != (blockRange{from: 1500, to: 2999}) {
		t.Fatalf("pending replay = %+v, want 1500-2999", s.pendingReplay)
	}

	// A second failure elsewhere merges into the pending range.
	chain.failFrom[100] = true
	_ = s.replay(context.Background(), blockRange{from: 100, to: 200})
	if *s.pendingReplay != (blockRange{from: 100, to: 2999}) {
		t.Fatalf("merged pending replay = %+v, want 100-2999", *s.pendingReplay)
	}

	s.retryPendingReplay(context.Background())
	if s.pendingReplay != nil {
		t.Fatalf("pending replay after a successful retry = %+v, want none", *s.pendingReplay)
	}
	last := chain.queried[len(chain.queried)-1]
	if last.to != 2999 {
		t.Fatalf("retry stopped at block %d, want 2999", last.to)
	}
}

// noWait replaces the service's pauses in tests.
func noWait(context.Context, time.Duration) error { return nil }

// newChainService returns a service that reads chain and does not pause.
func newChainService(q pgdb.Querier, chain *fakeChain) *Service {
	s := NewService(nil, q, logger.New(logger.Config{Level: slog.LevelError + 4}))
	s.chain = chain
	s.wait = noWait
	return s
}

// The startup replay reads the whole 24 hours in log queries of at most 100
// blocks each, which free RPC plans accept.
func TestReplayQueriesAtMost100Blocks(t *testing.T) {
	chain := &fakeChain{}
	s := newChainService(nil, chain)

	r := lookbackRange(50_000, startupSyncBlocks)
	if err := s.replay(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	next := r.from
	for _, q := range chain.queried {
		if q.from != next || q.to < q.from || q.to-q.from+1 > 100 {
			t.Fatalf("query %d-%d: want a range of at most 100 blocks from %d", q.from, q.to, next)
		}
		next = q.to + 1
	}
	if next != r.to+1 {
		t.Fatalf("queries ended at block %d, want %d", next-1, r.to)
	}
}

// A replay pauses between its log queries, and a pause cut short by shutdown
// leaves the unread blocks pending.
func TestReplayPausesBetweenQueries(t *testing.T) {
	chain := &fakeChain{}
	s := newChainService(nil, chain)
	var waits []time.Duration
	s.wait = func(_ context.Context, d time.Duration) error {
		waits = append(waits, d)
		return nil
	}
	if err := s.replay(context.Background(), blockRange{from: 0, to: 999}); err != nil {
		t.Fatal(err)
	}
	if len(chain.queried) != 10 || len(waits) != 9 {
		t.Fatalf("%d queries and %d pauses, want 10 and 9", len(chain.queried), len(waits))
	}
	for _, d := range waits {
		if d != replayBatchPause {
			t.Fatalf("paused %v, want %v", d, replayBatchPause)
		}
	}

	chain.queried = nil
	pauses := 0
	s.wait = func(context.Context, time.Duration) error {
		if pauses++; pauses == 3 {
			return context.Canceled
		}
		return nil
	}
	if err := s.replay(context.Background(), blockRange{from: 0, to: 999}); !errors.Is(err, context.Canceled) {
		t.Fatalf("replay = %v, want the cancellation", err)
	}
	if len(chain.queried) != 3 || s.pendingReplay == nil || *s.pendingReplay != (blockRange{from: 300, to: 999}) {
		t.Fatalf("%d queries, pending %+v; want 3 queries and 300-999 pending", len(chain.queried), s.pendingReplay)
	}
}

// seenQuerier records which payment IDs the handler looked up; none is known.
type seenQuerier struct {
	pgdb.Querier
	seen chan string
}

func (q seenQuerier) GetFaiPaymentIntentByPaymentID(_ context.Context, id string) (pgdb.FaiPaymentIntent, error) {
	q.seen <- id
	return pgdb.FaiPaymentIntent{}, sql.ErrNoRows
}

// The listener subscribes before it reads the head it replays to, so a payment
// mined while the replay runs, which only the live subscription delivers, is
// still handled.
func TestListenerSubscribesBeforeReplay(t *testing.T) {
	sig := common.HexToHash("0x01")
	q := seenQuerier{seen: make(chan string, 16)}
	chain := &fakeChain{head: 1000}
	s := newChainService(q, chain)
	s.eventSignature = sig

	gap := types.Log{Topics: []common.Hash{sig}, Data: make([]byte, 128), TxHash: common.HexToHash("0x9")}
	gap.Data[31] = 0x42 // payment ID
	var once sync.Once
	chain.onFilter = func() {
		once.Do(func() {
			chain.mu.Lock()
			live := chain.live
			chain.mu.Unlock()
			live <- gap // mined during the replay; the replay itself does not see it
		})
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.StartEventListener(ctx) }()

	want := "0000000000000000000000000000000000000000000000000000000000000042"
	select {
	case id := <-q.seen:
		if id != want {
			t.Fatalf("handled payment %s, want %s", id, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the payment delivered during the replay was not handled")
	}
	cancel()
	<-done

	chain.mu.Lock()
	defer chain.mu.Unlock()
	if len(chain.calls) < 2 || chain.calls[0] != "subscribe" || chain.calls[1] != "head" {
		t.Fatalf("calls = %v, want subscribe before head", chain.calls)
	}
}

// When the subscription fails and resubscribing fails too, the listener returns
// the error (main restarts it) instead of panicking, and releases the failed
// subscription.
func TestListenerReturnsErrorWhenResubscribeFails(t *testing.T) {
	chain := &fakeChain{head: 10, subscribeErrs: map[int]error{1: errors.New("websocket closed")}}
	s := newChainService(nil, chain)
	s.eventSignature = common.HexToHash("0x01")

	done := make(chan error, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				done <- fmt.Errorf("panic: %v", r)
			}
		}()
		done <- s.StartEventListener(context.Background())
	}()

	var first *fakeSubscription
	for i := 0; i < 500 && first == nil; i++ {
		chain.mu.Lock()
		if len(chain.subs) > 0 {
			first = chain.subs[0]
		}
		chain.mu.Unlock()
		time.Sleep(time.Millisecond)
	}
	if first == nil {
		t.Fatal("listener did not subscribe")
	}
	first.errs <- errors.New("connection lost")

	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "failed to resubscribe") {
			t.Fatalf("listener returned %v, want the resubscribe error", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("listener did not return")
	}
	first.mu.Lock()
	defer first.mu.Unlock()
	if first.unsubscribed == 0 {
		t.Fatal("the failed subscription was not released")
	}
}

func TestResubscribeWait(t *testing.T) {
	for failures, want := range map[int]time.Duration{
		0: 0, 1: time.Second, 2: 2 * time.Second, 3: 4 * time.Second, 6: 32 * time.Second,
		7: time.Minute, 8: time.Minute, 100: time.Minute,
	} {
		if got := resubscribeWait(failures); got != want {
			t.Errorf("resubscribeWait(%d) = %v, want %v", failures, got, want)
		}
	}
}

// When subscriptions keep ending at once, as when the endpoint closes the
// connection, the listener waits longer before each resubscribe instead of
// reconnecting in a loop; a subscription that lasted a minute resets the wait.
func TestListenerBacksOffWhileTheSubscriptionKeepsFailing(t *testing.T) {
	chain := &fakeChain{head: 10}
	s := newChainService(nil, chain)
	s.eventSignature = common.HexToHash("0x01")

	var mu sync.Mutex
	clock := time.Date(2026, 11, 25, 20, 0, 0, 0, time.UTC)
	var waits []time.Duration
	s.now = func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return clock
	}
	s.wait = func(ctx context.Context, d time.Duration) error {
		mu.Lock()
		defer mu.Unlock()
		waits = append(waits, d)
		return ctx.Err()
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.StartEventListener(ctx) }()

	subAt := func(i int) *fakeSubscription {
		t.Helper()
		for range 5000 {
			chain.mu.Lock()
			if len(chain.subs) > i {
				sub := chain.subs[i]
				chain.mu.Unlock()
				return sub
			}
			chain.mu.Unlock()
			time.Sleep(time.Millisecond)
		}
		t.Fatalf("subscription %d was not made", i)
		return nil
	}
	for i := range 4 {
		subAt(i).errs <- errors.New("websocket: close 1006")
	}
	fifth := subAt(4)
	mu.Lock()
	clock = clock.Add(2 * time.Minute) // the fifth subscription lasts two minutes
	mu.Unlock()
	fifth.errs <- errors.New("websocket: close 1006")
	subAt(5).errs <- errors.New("websocket: close 1006")
	subAt(6)
	cancel()
	<-done

	mu.Lock()
	defer mu.Unlock()
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, time.Second}
	if fmt.Sprint(waits) != fmt.Sprint(want) {
		t.Fatalf("waits before resubscribing = %v, want %v", waits, want)
	}
}
