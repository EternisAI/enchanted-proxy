package fai

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/core/types"
)

// retryableError marks a failure worth trying again later: the database or the
// price service was unavailable. Problems with the payment itself (an unsupported
// token, too small an amount, an unknown product) are not retryable.
type retryableError struct{ err error }

func (e retryableError) Error() string { return e.err.Error() }
func (e retryableError) Unwrap() error { return e.err }

func retryable(err error) error { return retryableError{err: err} }

func isRetryable(err error) bool {
	var r retryableError
	return errors.As(err, &r)
}

const (
	settlementRetryInitialDelay = time.Minute
	settlementRetryMaxDelay     = time.Hour
	// Longer than a payment intent lives (24h), after which it can no longer be settled.
	settlementRetryGiveUpAfter = 25 * time.Hour
)

type settlementRetry struct {
	log       types.Log
	attempts  int
	firstFail time.Time
	next      time.Time
}

// settlementRetries holds payment events whose settlement failed for a transient
// reason, so they are tried again after they have left the historical replay window.
// It lives in memory; across a restart, the startup sync replays the last 24 hours
// of events, which covers every intent that can still be settled.
type settlementRetries struct {
	mu      sync.Mutex
	entries map[string]*settlementRetry
}

func newSettlementRetries() *settlementRetries {
	return &settlementRetries{entries: map[string]*settlementRetry{}}
}

func retryKey(l types.Log) string {
	return fmt.Sprintf("%s:%d", l.TxHash.Hex(), l.Index)
}

// add queues an event for a later attempt. An event already queued keeps its schedule.
func (r *settlementRetries) add(l types.Log, now time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := retryKey(l)
	if _, ok := r.entries[key]; ok {
		return
	}
	r.entries[key] = &settlementRetry{log: l, firstFail: now, next: now.Add(settlementRetryInitialDelay)}
}

// due returns the events whose next attempt is at or before now.
func (r *settlementRetries) due(now time.Time) []types.Log {
	r.mu.Lock()
	defer r.mu.Unlock()
	var logs []types.Log
	for _, e := range r.entries {
		if !e.next.After(now) {
			logs = append(logs, e.log)
		}
	}
	return logs
}

// result records the outcome of an attempt. It reports whether the event was
// dropped because it kept failing past the give-up time.
func (r *settlementRetries) result(l types.Log, err error, now time.Time) (gaveUp bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := retryKey(l)
	e, ok := r.entries[key]
	if !ok {
		return false
	}
	if err == nil || !isRetryable(err) {
		delete(r.entries, key)
		return false
	}
	if now.Sub(e.firstFail) >= settlementRetryGiveUpAfter {
		delete(r.entries, key)
		return true
	}
	e.attempts++
	delay := settlementRetryInitialDelay << e.attempts
	if delay <= 0 || delay > settlementRetryMaxDelay {
		delay = settlementRetryMaxDelay
	}
	e.next = now.Add(delay)
	return false
}

func (r *settlementRetries) len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.entries)
}
