package probe

import (
	"fmt"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	// probeRequestsTotal counts all probe attempts.
	probeRequestsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "model_router_probe_rq_total",
			Help: "Total probe request attempts, by provider and model.",
		},
		[]string{"provider", "model"},
	)

	// probeRequestsCompleted counts probe HTTP responses received, by status code.
	probeRequestsCompleted = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "model_router_probe_rq_completed",
			Help: "Total probe responses received, by provider, model, and HTTP status code.",
		},
		[]string{"provider", "model", "response_code"},
	)

	// probeRequestsSuccess counts successful probe requests (2xx HTTP status).
	probeRequestsSuccess = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "model_router_probe_rq_success",
			Help: "Total successful probe requests (2xx), by provider and model.",
		},
		[]string{"provider", "model"},
	)

	// probeResponseExpected counts successful probes where response content matched expectations.
	probeResponseExpected = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "model_router_probe_response_expected",
			Help: "Total probe responses with expected content, by provider and model.",
		},
		[]string{"provider", "model"},
	)

	// probeResponseUnexpected counts successful probes (2xx) where response content did not match expectations.
	probeResponseUnexpected = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "model_router_probe_response_unexpected",
			Help: "Total probe responses with unexpected content, by provider and model.",
		},
		[]string{"provider", "model"},
	)

	// probeRequestTime observes probe request duration in seconds.
	probeRequestTime = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "model_router_probe_rq_time",
			Help:    "Probe request duration in seconds, by provider and model.",
			Buckets: []float64{0.1, 0.25, 0.5, 0.75, 1, 1.5, 2, 2.5, 5, 10, 30},
		},
		[]string{"provider", "model"},
	)

	// probePromptTokensTotal accumulates prompt tokens consumed by probes.
	probePromptTokensTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "model_router_probe_prompt_tokens_total",
			Help: "Total prompt tokens consumed by probe requests, by provider and model.",
		},
		[]string{"provider", "model"},
	)

	// probeCompletionTokensTotal accumulates completion tokens consumed by probes.
	probeCompletionTokensTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "model_router_probe_completion_tokens_total",
			Help: "Total completion tokens consumed by probe requests, by provider and model.",
		},
		[]string{"provider", "model"},
	)
)

// probeTokenUsage holds token counts extracted from a probe response.
type probeTokenUsage struct {
	PromptTokens     int
	CompletionTokens int
}

// recordProbeResult records all metrics for a completed probe attempt.
// statusCode of 0 indicates a connection failure (no HTTP response received).
// contentMatch indicates whether the response content matched expectations.
// hasExpectedResponse indicates whether content checking was configured.
// usage contains token counts from the response (nil if unavailable).
func recordProbeResult(provider, model string, statusCode int, durationSeconds float64, contentMatch bool, hasExpectedResponse bool, usage *probeTokenUsage) {
	probeRequestsTotal.WithLabelValues(provider, model).Inc()
	probeRequestTime.WithLabelValues(provider, model).Observe(durationSeconds)

	if statusCode > 0 {
		probeRequestsCompleted.WithLabelValues(provider, model, fmt.Sprintf("%d", statusCode)).Inc()
	}

	if statusCode >= 200 && statusCode < 300 {
		probeRequestsSuccess.WithLabelValues(provider, model).Inc()

		if hasExpectedResponse {
			if contentMatch {
				probeResponseExpected.WithLabelValues(provider, model).Inc()
			} else {
				probeResponseUnexpected.WithLabelValues(provider, model).Inc()
			}
		}
	}

	if usage != nil {
		if usage.PromptTokens > 0 {
			probePromptTokensTotal.WithLabelValues(provider, model).Add(float64(usage.PromptTokens))
		}
		if usage.CompletionTokens > 0 {
			probeCompletionTokensTotal.WithLabelValues(provider, model).Add(float64(usage.CompletionTokens))
		}
	}
}
