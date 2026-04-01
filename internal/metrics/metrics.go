package metrics

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	// UpstreamRequestsTotal counts all attempts to route a request upstream,
	// regardless of whether the HTTP request succeeds or fails.
	UpstreamRequestsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "model_router_upstream_rq_total",
			Help: "Total upstream request attempts, by provider and model.",
		},
		[]string{"provider", "model"},
	)

	// UpstreamRequestsCompleted counts HTTP responses received from upstream providers.
	UpstreamRequestsCompleted = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "model_router_upstream_rq_completed",
			Help: "Total upstream responses received, by provider, model, and HTTP status code.",
		},
		[]string{"provider", "model", "response_code"},
	)

	// UpstreamRequests1xx counts upstream responses with 1xx status codes.
	UpstreamRequests1xx = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "model_router_upstream_rq_1xx",
			Help: "Total upstream 1xx responses, by provider and model.",
		},
		[]string{"provider", "model"},
	)

	// UpstreamRequests2xx counts upstream responses with 2xx status codes.
	UpstreamRequests2xx = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "model_router_upstream_rq_2xx",
			Help: "Total upstream 2xx responses, by provider and model.",
		},
		[]string{"provider", "model"},
	)

	// UpstreamRequests3xx counts upstream responses with 3xx status codes.
	UpstreamRequests3xx = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "model_router_upstream_rq_3xx",
			Help: "Total upstream 3xx responses, by provider and model.",
		},
		[]string{"provider", "model"},
	)

	// UpstreamRequests4xx counts upstream responses with 4xx status codes.
	UpstreamRequests4xx = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "model_router_upstream_rq_4xx",
			Help: "Total upstream 4xx responses, by provider and model.",
		},
		[]string{"provider", "model"},
	)

	// UpstreamRequests5xx counts upstream responses with 5xx status codes.
	UpstreamRequests5xx = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "model_router_upstream_rq_5xx",
			Help: "Total upstream 5xx responses, by provider and model.",
		},
		[]string{"provider", "model"},
	)

	// UpstreamRequestTime observes upstream request duration in seconds.
	UpstreamRequestTime = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "model_router_upstream_rq_time",
			Help:    "Upstream request duration in seconds, by provider and model.",
			Buckets: []float64{0.1, 0.25, 0.5, 0.75, 1, 1.5, 2, 2.5, 5, 10, 30, 60, 120},
		},
		[]string{"provider", "model"},
	)

	// UpstreamRequestsActive tracks the number of upstream requests currently in progress.
	UpstreamRequestsActive = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "model_router_upstream_rq_active",
			Help: "Number of upstream requests currently in progress.",
		},
		[]string{"provider", "model"},
	)

	// UpstreamRequestTimeouts counts response-phase timeouts (e.g. ResponseHeaderTimeout)
	// that occur after a connection was established and the HTTP request was sent.
	UpstreamRequestTimeouts = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "model_router_upstream_rq_timeout",
			Help: "Total upstream response timeouts (after connection established).",
		},
		[]string{"provider", "model"},
	)

	// ConnectFailures counts any connection failure attempting to reach the upstream provider,
	// including dial timeouts, TLS failures, DNS errors, etc.
	ConnectFailures = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "model_router_cx_connect_fail",
			Help: "Total upstream connection failures.",
		},
		[]string{"provider", "model"},
	)

	// ConnectTimeouts counts connection-phase timeouts specifically (dial timeout, TLS handshake timeout).
	ConnectTimeouts = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "model_router_cx_connect_timeout",
			Help: "Total upstream connection-phase timeouts (dial, TLS handshake).",
		},
		[]string{"provider", "model"},
	)

	// --- Active health probe metrics ---

	// ProbeRequestsTotal counts all probe attempts.
	ProbeRequestsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "model_router_probe_rq_total",
			Help: "Total probe request attempts, by provider and model.",
		},
		[]string{"provider", "model"},
	)

	// ProbeRequestsCompleted counts probe HTTP responses received, by status code.
	ProbeRequestsCompleted = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "model_router_probe_rq_completed",
			Help: "Total probe responses received, by provider, model, and HTTP status code.",
		},
		[]string{"provider", "model", "response_code"},
	)

	// ProbeRequestsSuccess counts successful probe requests (2xx HTTP status).
	ProbeRequestsSuccess = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "model_router_probe_rq_success",
			Help: "Total successful probe requests (2xx), by provider and model.",
		},
		[]string{"provider", "model"},
	)

	// ProbeResponseExpected counts successful probes where response content matched expectations.
	ProbeResponseExpected = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "model_router_probe_response_expected",
			Help: "Total probe responses with expected content, by provider and model.",
		},
		[]string{"provider", "model"},
	)

	// ProbeResponseUnexpected counts successful probes (2xx) where response content did not match expectations.
	ProbeResponseUnexpected = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "model_router_probe_response_unexpected",
			Help: "Total probe responses with unexpected content, by provider and model.",
		},
		[]string{"provider", "model"},
	)

	// ProbeRequestTime observes probe request duration in seconds.
	ProbeRequestTime = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "model_router_probe_rq_time",
			Help:    "Probe request duration in seconds, by provider and model.",
			Buckets: []float64{0.1, 0.25, 0.5, 0.75, 1, 1.5, 2, 2.5, 5, 10, 30},
		},
		[]string{"provider", "model"},
	)

	// ProbePromptTokensTotal accumulates prompt tokens consumed by probes.
	ProbePromptTokensTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "model_router_probe_prompt_tokens_total",
			Help: "Total prompt tokens consumed by probe requests, by provider and model.",
		},
		[]string{"provider", "model"},
	)

	// ProbeCompletionTokensTotal accumulates completion tokens consumed by probes.
	ProbeCompletionTokensTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "model_router_probe_completion_tokens_total",
			Help: "Total completion tokens consumed by probe requests, by provider and model.",
		},
		[]string{"provider", "model"},
	)
)

// isTimeout returns true if err represents any kind of timeout.
func isTimeout(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	return false
}

// isDialError returns true if the error chain contains a dial-phase net.OpError.
func isDialError(err error) bool {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		var opErr *net.OpError
		if errors.As(urlErr.Err, &opErr) && opErr.Op == "dial" {
			return true
		}
	}
	return false
}

// RecordUpstreamError classifies a connection/request error and increments the appropriate counters.
//
// Classification:
//  1. Dial-phase timeout (e.g. dial timeout, TLS handshake timeout): increments cx_connect_fail + cx_connect_timeout
//  2. Dial-phase non-timeout error (e.g. connection refused, DNS failure): increments cx_connect_fail only
//  3. Response-phase timeout (e.g. ResponseHeaderTimeout): increments rq_timeout only
func RecordUpstreamError(provider, model string, err error) {
	if isDialError(err) {
		ConnectFailures.WithLabelValues(provider, model).Inc()
		if isTimeout(err) {
			ConnectTimeouts.WithLabelValues(provider, model).Inc()
		}
	} else if isTimeout(err) {
		UpstreamRequestTimeouts.WithLabelValues(provider, model).Inc()
	} else {
		ConnectFailures.WithLabelValues(provider, model).Inc()
	}
}

// RecordUpstreamAttempt records that we are about to attempt an upstream request.
// Call this after routing is complete, before any connection attempt.
func RecordUpstreamAttempt(provider, model string) {
	UpstreamRequestsTotal.WithLabelValues(provider, model).Inc()
}

// RecordUpstreamResponse records an HTTP response from upstream and increments
// the completed counter plus the appropriate Nxx response-class counter.
// durationSeconds is the time from request start to first response byte.
func RecordUpstreamResponse(provider, model string, statusCode int, durationSeconds float64) {
	UpstreamRequestsCompleted.WithLabelValues(provider, model, fmt.Sprintf("%d", statusCode)).Inc()
	UpstreamRequestTime.WithLabelValues(provider, model).Observe(durationSeconds)

	switch {
	case statusCode >= 100 && statusCode < 200:
		UpstreamRequests1xx.WithLabelValues(provider, model).Inc()
	case statusCode >= 200 && statusCode < 300:
		UpstreamRequests2xx.WithLabelValues(provider, model).Inc()
	case statusCode >= 300 && statusCode < 400:
		UpstreamRequests3xx.WithLabelValues(provider, model).Inc()
	case statusCode >= 400 && statusCode < 500:
		UpstreamRequests4xx.WithLabelValues(provider, model).Inc()
	case statusCode >= 500 && statusCode < 600:
		UpstreamRequests5xx.WithLabelValues(provider, model).Inc()
	}
}

// ProbeTokenUsage holds token counts extracted from a probe response.
type ProbeTokenUsage struct {
	PromptTokens     int
	CompletionTokens int
}

// RecordProbeResult records all metrics for a completed probe attempt.
// statusCode of 0 indicates a connection failure (no HTTP response received).
// contentMatch indicates whether the response content matched expectations.
// hasExpectedResponse indicates whether content checking was configured.
// usage contains token counts from the response (nil if unavailable).
func RecordProbeResult(provider, model string, statusCode int, durationSeconds float64, contentMatch bool, hasExpectedResponse bool, usage *ProbeTokenUsage) {
	ProbeRequestsTotal.WithLabelValues(provider, model).Inc()
	ProbeRequestTime.WithLabelValues(provider, model).Observe(durationSeconds)

	if statusCode > 0 {
		ProbeRequestsCompleted.WithLabelValues(provider, model, fmt.Sprintf("%d", statusCode)).Inc()
	}

	if statusCode >= 200 && statusCode < 300 {
		ProbeRequestsSuccess.WithLabelValues(provider, model).Inc()

		if hasExpectedResponse {
			if contentMatch {
				ProbeResponseExpected.WithLabelValues(provider, model).Inc()
			} else {
				ProbeResponseUnexpected.WithLabelValues(provider, model).Inc()
			}
		}
	}

	if usage != nil {
		if usage.PromptTokens > 0 {
			ProbePromptTokensTotal.WithLabelValues(provider, model).Add(float64(usage.PromptTokens))
		}
		if usage.CompletionTokens > 0 {
			ProbeCompletionTokensTotal.WithLabelValues(provider, model).Add(float64(usage.CompletionTokens))
		}
	}
}

// TrackActiveRequest increments the active request gauge and returns a function
// that decrements it. Intended for use with defer.
func TrackActiveRequest(provider, model string) func() {
	gauge := UpstreamRequestsActive.WithLabelValues(provider, model)
	gauge.Inc()
	return func() {
		gauge.Dec()
	}
}
