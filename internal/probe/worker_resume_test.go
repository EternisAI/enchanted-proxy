package probe

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/eternisai/enchanted-proxy/internal/config"
	"github.com/eternisai/enchanted-proxy/internal/routing"
)

// slackRecorder stands in for the Slack webhook and records what the worker sends.
type slackRecorder struct {
	server *httptest.Server

	mu       sync.Mutex
	messages []slackMessage
}

func newSlackRecorder(t *testing.T) *slackRecorder {
	t.Helper()
	r := &slackRecorder{}
	r.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var msg slackMessage
		_ = json.NewDecoder(req.Body).Decode(&msg)
		r.mu.Lock()
		r.messages = append(r.messages, msg)
		r.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(r.server.Close)
	return r
}

func (r *slackRecorder) received() []slackMessage {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]slackMessage(nil), r.messages...)
}

// awaitMessages waits for at least n notifications, returning what arrived.
func (r *slackRecorder) awaitMessages(n int, timeout time.Duration) []slackMessage {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if got := r.received(); len(got) >= n {
			return got
		}
		time.Sleep(5 * time.Millisecond)
	}
	return r.received()
}

// newProbeEndpoint starts a fake model endpoint. Each call to respond decides the
// status and content of the next probe response.
func newProbeEndpoint(t *testing.T, status int, content string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{
				{"message": map[string]string{"content": content}},
			},
			"usage": map[string]int{"prompt_tokens": 3, "completion_tokens": 1},
		})
	}))
	t.Cleanup(server.Close)
	return server
}

// newTestWorker assembles a worker against a fake endpoint and Slack webhook, with
// intervals short enough to keep tests fast and thresholds of 1 so a single result
// drives a transition.
func newTestWorker(t *testing.T, model string, endpoint *httptest.Server, slack *slackRecorder, restored *targetState) *probeWorker {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	service := &ProbeService{
		logger:   testLogger(),
		slack:    newSlackNotifier(slack.server.URL),
		shutdown: make(chan struct{}),
		cancel:   cancel,
	}
	t.Cleanup(service.Shutdown)

	expected := "OK"
	provider := &routing.ProviderConfig{
		BaseURL: endpoint.URL,
		APIKey:  "test-key",
		Name:    "TestProvider",
		Model:   model,
		APIType: config.APITypeChatCompletions,
	}

	return &probeWorker{
		service:  service,
		ctx:      ctx,
		provider: "TestProvider",
		model:    model,
		endpoint: provider,
		probe: &routing.ProbeConfig{
			Enabled:          true,
			Interval:         50 * time.Millisecond,
			RetryInterval:    20 * time.Millisecond,
			Prompt:           "Say OK",
			ExpectedResponse: &expected,
			MaxTokens:        16,
			SuccessThreshold: 1,
			FailureThreshold: 1,
		},
		client:   endpoint.Client(),
		logger:   service.logger,
		slack:    service.slack,
		key:      newTargetKey("TestProvider", model, endpoint.URL, model),
		restored: restored,
	}
}

func start(t *testing.T, w *probeWorker) {
	t.Helper()
	w.service.wg.Add(1)
	go w.run()
}

// failingState returns a persisted record for a target that was failing when the
// previous process recorded it — the situation an operator restart is meant to fix.
func failingState(w *probeWorker, age time.Duration) *targetState {
	at := time.Now().Add(-age)
	return &targetState{
		Version:        stateSchemaVersion,
		Provider:       w.key.Provider,
		CanonicalModel: w.key.CanonicalModel,
		BaseURL:        w.key.BaseURL,
		EffectiveModel: w.key.EffectiveModel,
		State:          stateFailing,
		StateChangedAt: at,
		LastProbeAt:    at,
	}
}

// TestResumedFailingWorkerAnnouncesRecovery covers the bug this state exists for:
// an endpoint that was failing before a restart — typically restarted to install a
// replacement API key — must announce its recovery, not absorb it as an initial
// state.
func TestResumedFailingWorkerAnnouncesRecovery(t *testing.T) {
	endpoint := newProbeEndpoint(t, http.StatusOK, "OK")
	slack := newSlackRecorder(t)

	w := newTestWorker(t, "resumed-recovery", endpoint, slack, nil)
	w.restored = failingState(w, time.Hour)

	start(t, w)

	messages := slack.awaitMessages(1, 3*time.Second)
	if len(messages) == 0 {
		t.Fatal("no recovery notification was sent for an endpoint that was failing before the restart")
	}
	if messages[0].Text != "Probe succeeded: TestProvider / resumed-recovery" {
		t.Errorf("unexpected notification text: %q", messages[0].Text)
	}
}

// TestFreshWorkerIsSilentOnInitialSuccess pins the existing behaviour that the
// resume path deliberately bypasses: with no prior state there is nothing to
// recover from, so establishing an initial healthy state stays quiet.
func TestFreshWorkerIsSilentOnInitialSuccess(t *testing.T) {
	endpoint := newProbeEndpoint(t, http.StatusOK, "OK")
	slack := newSlackRecorder(t)

	w := newTestWorker(t, "fresh-healthy", endpoint, slack, nil)
	start(t, w)

	if got := slack.awaitMessages(1, 300*time.Millisecond); len(got) != 0 {
		t.Errorf("establishing an initial healthy state sent %d notification(s), want 0", len(got))
	}
}

// TestResumedFailingWorkerDoesNotRepeatItsFailure ensures resuming is silent about
// the state it inherited: that transition was already announced by the process
// that recorded it, and re-announcing it on every restart would be noise.
func TestResumedFailingWorkerDoesNotRepeatItsFailure(t *testing.T) {
	endpoint := newProbeEndpoint(t, http.StatusInternalServerError, "")
	slack := newSlackRecorder(t)

	w := newTestWorker(t, "resumed-still-failing", endpoint, slack, nil)
	w.restored = failingState(w, time.Hour)

	start(t, w)

	if got := slack.awaitMessages(1, 300*time.Millisecond); len(got) != 0 {
		t.Errorf("a still-failing resumed endpoint sent %d notification(s), want 0", len(got))
	}
}

// TestResumedHealthyWorkerStillAlerts confirms resuming does not disarm alerting:
// a target restored as healthy must still report a failure it runs into.
func TestResumedHealthyWorkerStillAlerts(t *testing.T) {
	endpoint := newProbeEndpoint(t, http.StatusInternalServerError, "")
	slack := newSlackRecorder(t)

	w := newTestWorker(t, "resumed-healthy", endpoint, slack, nil)
	state := failingState(w, time.Hour)
	state.State = stateHealthy
	w.restored = state

	start(t, w)

	messages := slack.awaitMessages(1, 3*time.Second)
	if len(messages) == 0 {
		t.Fatal("no failure notification was sent for an endpoint restored as healthy")
	}
	if messages[0].Text != "Probe failed: TestProvider / resumed-healthy" {
		t.Errorf("unexpected notification text: %q", messages[0].Text)
	}
}

func TestResumeDelay(t *testing.T) {
	const (
		interval      = 15 * time.Minute
		retryInterval = time.Minute
		jitter        = 10 * time.Second
	)

	w := &probeWorker{probe: &routing.ProbeConfig{Interval: interval, RetryInterval: retryInterval}}

	tests := []struct {
		name        string
		state       healthState
		lastProbe   time.Duration // age of LastProbeAt; 0 means unset
		stateChange time.Duration // age of StateChangedAt
		want        time.Duration // expected delay excluding jitter
	}{
		{
			name:        "healthy mid-interval waits out the remainder",
			state:       stateHealthy,
			lastProbe:   5 * time.Minute,
			stateChange: time.Hour,
			want:        10 * time.Minute,
		},
		{
			name:        "healthy with an elapsed interval probes at once",
			state:       stateHealthy,
			lastProbe:   30 * time.Minute,
			stateChange: time.Hour,
			want:        0,
		},
		{
			name:        "failing uses the retry interval, not the probe interval",
			state:       stateFailing,
			lastProbe:   30 * time.Second,
			stateChange: time.Hour,
			want:        30 * time.Second,
		},
		{
			name:        "failing with an elapsed retry interval probes at once",
			state:       stateFailing,
			lastProbe:   5 * time.Minute,
			stateChange: time.Hour,
			want:        0,
		},
		{
			name:        "falls back to the state change when no probe was flushed",
			state:       stateHealthy,
			lastProbe:   0,
			stateChange: 5 * time.Minute,
			want:        10 * time.Minute,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			state := targetState{
				State:          tt.state,
				StateChangedAt: time.Now().Add(-tt.stateChange),
			}
			if tt.lastProbe > 0 {
				state.LastProbeAt = time.Now().Add(-tt.lastProbe)
			}

			got := w.resumeDelay(state, jitter)

			// Tolerance absorbs the wall clock advancing during the call.
			if diff := got - (tt.want + jitter); diff < -time.Second || diff > time.Second {
				t.Errorf("resumeDelay = %s, want ~%s", got, tt.want+jitter)
			}
		})
	}
}

// TestResumeDelayClampsFutureTimestamps guards against a clock that moved
// backwards between processes pushing the first probe beyond a full interval.
func TestResumeDelayClampsFutureTimestamps(t *testing.T) {
	w := &probeWorker{probe: &routing.ProbeConfig{Interval: 15 * time.Minute, RetryInterval: time.Minute}}

	state := targetState{
		State:          stateHealthy,
		StateChangedAt: time.Now(),
		LastProbeAt:    time.Now().Add(2 * time.Hour),
	}

	if got := w.resumeDelay(state, 0); got > 15*time.Minute {
		t.Errorf("resumeDelay = %s, want at most one interval (15m)", got)
	}
}
