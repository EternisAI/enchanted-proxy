package fai

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/eternisai/enchanted-proxy/internal/logger"
	pgdb "github.com/eternisai/enchanted-proxy/internal/storage/pg/sqlc"
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
