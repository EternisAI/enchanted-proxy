package probe

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/eternisai/enchanted-proxy/internal/config"
	"github.com/eternisai/enchanted-proxy/internal/routing"
)

// newSlowProbeEndpoint starts a fake model endpoint that waits before sending
// anything at all — no status line, no headers — which is how a real endpoint
// behaves while a non-streaming completion is still generating.
func newSlowProbeEndpoint(t *testing.T, delay time.Duration) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(delay):
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{
				{"message": map[string]string{"content": "OK"}},
			},
		})
	}))
	t.Cleanup(server.Close)
	return server
}

func TestNewProbeHTTPClientDefaultsToConfigDefault(t *testing.T) {
	for _, timeout := range []time.Duration{0, -time.Second} {
		client := newProbeHTTPClient(timeout)
		if client.Timeout != config.DefaultProbeTimeout {
			t.Errorf("timeout %v: got client timeout %v, want %v",
				timeout, client.Timeout, config.DefaultProbeTimeout)
		}
	}
}

// The response-header timeout must not be stricter than the overall budget:
// a probe is non-streaming, so headers arrive only once generation is done, and
// a shorter header limit would be the only deadline that ever applied.
func TestProbeHTTPClientHeaderTimeoutMatchesBudget(t *testing.T) {
	client := newProbeHTTPClient(90 * time.Second)
	transport, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("unexpected transport type %T", client.Transport)
	}
	if transport.ResponseHeaderTimeout != 90*time.Second {
		t.Errorf("got response header timeout %v, want %v",
			transport.ResponseHeaderTimeout, 90*time.Second)
	}
}

// Connection setup must never be allowed to eat a budget smaller than itself.
func TestProbeHTTPClientCapsConnectTimeoutToBudget(t *testing.T) {
	client := newProbeHTTPClient(2 * time.Second)
	transport, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("unexpected transport type %T", client.Transport)
	}
	if transport.TLSHandshakeTimeout != 2*time.Second {
		t.Errorf("got TLS handshake timeout %v, want %v",
			transport.TLSHandshakeTimeout, 2*time.Second)
	}
}

// A probe waiting on a slow endpoint fails once its own budget is spent, and
// succeeds when the budget covers the wait — the flapping this configurability
// exists to fix.
func TestProbeRespectsConfiguredTimeout(t *testing.T) {
	const responseDelay = 300 * time.Millisecond

	tests := []struct {
		name        string
		timeout     time.Duration
		wantSuccess bool
	}{
		{name: "budget shorter than the endpoint", timeout: 50 * time.Millisecond},
		{name: "budget covers the endpoint", timeout: 3 * time.Second, wantSuccess: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			endpoint := newSlowProbeEndpoint(t, responseDelay)
			expected := "OK"
			w := &probeWorker{
				ctx: context.Background(),
				endpoint: &routing.ProviderConfig{
					BaseURL: endpoint.URL,
					Name:    "TestProvider",
					Model:   "slow-model",
					APIType: config.APITypeChatCompletions,
				},
				probe: &routing.ProbeConfig{
					Prompt:           "Say OK",
					ExpectedResponse: &expected,
					MaxTokens:        16,
					Timeout:          tt.timeout,
				},
				provider: "TestProvider",
				model:    "slow-model",
				client:   newProbeHTTPClient(tt.timeout),
				logger:   testLogger(),
			}

			result := w.runProbe()

			if result.success != tt.wantSuccess {
				t.Fatalf("got success=%v (err=%v), want success=%v",
					result.success, result.err, tt.wantSuccess)
			}
			if tt.wantSuccess {
				return
			}
			if result.err == nil {
				t.Fatal("expected a timeout error, got none")
			}
			var timeoutErr interface{ Timeout() bool }
			if !errors.As(result.err, &timeoutErr) || !timeoutErr.Timeout() {
				t.Fatalf("expected a timeout error, got %v", result.err)
			}
		})
	}
}

// A probe that outruns its own retry interval must still leave that interval
// idle before the next attempt. The scheduler measures the wait from the end of
// one probe, so a slow endpoint delays the next probe instead of being hit back
// to back for as long as it stays down.
func TestSlowProbesLeaveTheRetryIntervalIdle(t *testing.T) {
	const (
		probeTimeout  = 150 * time.Millisecond
		retryInterval = 100 * time.Millisecond // deliberately shorter than the timeout
	)

	arrivals := make(chan time.Time, 8)
	// Never answers: every probe against it runs its full budget and times out.
	// The client hanging up does not reliably unblock the handler, so shutting
	// the server down has to release it explicitly — hence a channel closed by a
	// cleanup registered after (and so running before) the server's own.
	release := make(chan struct{})
	endpoint := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case arrivals <- time.Now():
		default:
		}
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(endpoint.Close)
	t.Cleanup(func() { close(release) })

	w := newTestWorker(t, "slow-model", endpoint, newSlackRecorder(t), nil)
	w.probe.Interval = 400 * time.Millisecond
	w.probe.RetryInterval = retryInterval
	w.probe.Timeout = probeTimeout
	w.client = newProbeHTTPClient(probeTimeout)
	start(t, w)

	const want = 3
	seen := make([]time.Time, 0, want)
	for len(seen) < want {
		select {
		case at := <-arrivals:
			seen = append(seen, at)
		case <-time.After(5 * time.Second):
			t.Fatalf("got %d probes, want %d", len(seen), want)
		}
	}

	// The probe itself accounts for probeTimeout of every gap; what is left is
	// the idle time the retry interval is supposed to guarantee. Scheduling is
	// not exact, so allow the wait to come up a little short.
	minGap := probeTimeout + retryInterval*7/10
	for i := 1; i < len(seen); i++ {
		if gap := seen[i].Sub(seen[i-1]); gap < minGap {
			t.Errorf("probe %d started %v after probe %d, want at least %v",
				i+1, gap, i, minGap)
		}
	}
}
