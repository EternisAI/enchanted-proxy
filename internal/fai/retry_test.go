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
	s := NewService(nil, nil, logger.New(logger.Config{Level: slog.LevelError + 4}))
	s.chain = chain

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
	s := NewService(nil, q, logger.New(logger.Config{Level: slog.LevelError + 4}))
	s.eventSignature = sig

	gap := types.Log{Topics: []common.Hash{sig}, Data: make([]byte, 128), TxHash: common.HexToHash("0x9")}
	gap.Data[31] = 0x42 // payment ID
	chain := &fakeChain{head: 1000}
	var once sync.Once
	chain.onFilter = func() {
		once.Do(func() {
			chain.mu.Lock()
			live := chain.live
			chain.mu.Unlock()
			live <- gap // mined during the replay; the replay itself does not see it
		})
	}
	s.chain = chain

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
	s := NewService(nil, nil, logger.New(logger.Config{Level: slog.LevelError + 4}))
	s.eventSignature = common.HexToHash("0x01")
	chain := &fakeChain{head: 10, subscribeErrs: map[int]error{1: errors.New("websocket closed")}}
	s.chain = chain

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
