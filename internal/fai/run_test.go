package fai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/gin-gonic/gin"
)

// recordWaits makes s record its pauses and return at once, or with ctx's
// error once ctx has ended.
func recordWaits(s *Service) func() []time.Duration {
	var mu sync.Mutex
	var waits []time.Duration
	s.wait = func(ctx context.Context, d time.Duration) error {
		mu.Lock()
		defer mu.Unlock()
		waits = append(waits, d)
		return ctx.Err()
	}
	return func() []time.Duration {
		mu.Lock()
		defer mu.Unlock()
		return append([]time.Duration(nil), waits...)
	}
}

// waitForSubscriptions waits until chain has had n subscribe calls.
func waitForSubscriptions(t *testing.T, chain *fakeChain, n int) {
	t.Helper()
	for range 5000 {
		chain.mu.Lock()
		got := len(chain.subs)
		chain.mu.Unlock()
		if got >= n {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("fewer than %d subscriptions were made", n)
}

// An instance whose chain connection is refused keeps retrying, waiting longer
// each time up to five minutes, and starts the listener once it connects.
func TestRunRetriesTheConnectionUntilItSucceeds(t *testing.T) {
	chain := &fakeChain{head: 10}
	s := newChainService(nil, chain)
	s.eventSignature = common.HexToHash("0x01")
	waits := recordWaits(s)
	attempts := 0
	s.connect = func(string, string) error {
		if attempts++; attempts <= 8 {
			return errors.New("failed to get chain ID: rate limit")
		}
		return nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); s.Run(ctx, "wss://example", "0x02") }()
	waitForSubscriptions(t, chain, 1)
	cancel()
	<-done

	want := []time.Duration{5 * time.Second, 10 * time.Second, 20 * time.Second, 40 * time.Second,
		80 * time.Second, 160 * time.Second, 5 * time.Minute, 5 * time.Minute}
	if got := waits(); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("waits between connection attempts = %v, want %v", got, want)
	}
	if !s.Ready() {
		t.Fatal("not ready after connecting")
	}
}

// Shutting down while the connection is being retried ends Run without
// starting the listener.
func TestRunStopsWhileRetryingTheConnection(t *testing.T) {
	chain := &fakeChain{head: 10}
	s := newChainService(nil, chain)
	ctx, cancel := context.WithCancel(context.Background())
	attempts := 0
	s.connect = func(string, string) error {
		if attempts++; attempts == 2 {
			cancel()
		}
		return errors.New("connection refused")
	}
	waits := recordWaits(s)

	done := make(chan struct{})
	go func() { defer close(done); s.Run(ctx, "wss://example", "0x02") }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancellation")
	}
	if attempts != 2 || len(waits()) != 2 {
		t.Fatalf("%d attempts and %d waits, want 2 and 2", attempts, len(waits()))
	}
	chain.mu.Lock()
	defer chain.mu.Unlock()
	if len(chain.subs) != 0 {
		t.Fatalf("%d subscriptions made, want none", len(chain.subs))
	}
}

// A listener that stops with an error is started again after five seconds.
func TestRunRestartsTheListener(t *testing.T) {
	chain := &fakeChain{head: 10, subscribeErrs: map[int]error{0: errors.New("rate limit")}}
	s := newChainService(nil, chain)
	s.eventSignature = common.HexToHash("0x01")
	s.connect = func(string, string) error { return nil }
	waits := recordWaits(s)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); s.Run(ctx, "wss://example", "0x02") }()
	waitForSubscriptions(t, chain, 2)
	cancel()
	<-done

	if got := waits(); len(got) != 1 || got[0] != listenerRestartWait {
		t.Fatalf("waits = %v, want one restart wait of %v", got, listenerRestartWait)
	}
}

// The config route answers 503 until the chain connection is made, then the
// connection's details.
func TestConfigWaitsForTheChainConnection(t *testing.T) {
	gin.SetMode(gin.TestMode)
	s := newChainService(nil, &fakeChain{})
	h := NewHandler(s, s.logger)

	get := func() *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/fai/config", nil)
		h.GetConfig(c)
		return w
	}
	if w := get(); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("before connecting: %d %s, want 503", w.Code, w.Body)
	}

	s.contractAddress = common.HexToAddress("0x02")
	s.chainID = 8453
	s.tokenConfigs = map[string]TokenConfig{"0x03": {CoingeckoID: "freysa-ai"}}
	s.ready.Store(true)
	w := get()
	var cfg ConfigResponse
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &cfg) != nil || cfg.ChainID != 8453 || cfg.FaiTokenAddress != "0x03" {
		t.Fatalf("after connecting: %d %s", w.Code, w.Body)
	}
}
