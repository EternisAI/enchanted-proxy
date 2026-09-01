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
