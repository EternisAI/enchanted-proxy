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
	// UpstreamRequestsTotal counts HTTP responses received from upstream providers.
	UpstreamRequestsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "model_router_upstream_rq_total",
			Help: "Total upstream responses received, by provider, model, and HTTP status code.",
		},
		[]string{"provider", "model", "response_code"},
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
//  1. Response-phase timeout (e.g. ResponseHeaderTimeout): increments rq_timeout + cx_connect_fail
//  2. Connect-phase timeout (e.g. dial timeout): increments cx_connect_fail + cx_connect_timeout
//  3. Other connection failure: increments cx_connect_fail only
func RecordUpstreamError(provider, model string, err error) {
	ConnectFailures.WithLabelValues(provider, model).Inc()

	if isTimeout(err) {
		if isDialError(err) {
			ConnectTimeouts.WithLabelValues(provider, model).Inc()
		} else {
			UpstreamRequestTimeouts.WithLabelValues(provider, model).Inc()
		}
	}
}

// RecordUpstreamResponse records a successful HTTP response from upstream.
func RecordUpstreamResponse(provider, model string, statusCode int) {
	UpstreamRequestsTotal.WithLabelValues(provider, model, fmt.Sprintf("%d", statusCode)).Inc()
}

// TrackActiveRequest increments the active request gauge and returns a function
// that decrements it. Intended for use with defer.
func TrackActiveRequest(provider, model string) func() {
	UpstreamRequestsActive.WithLabelValues(provider, model).Inc()
	return func() {
		UpstreamRequestsActive.WithLabelValues(provider, model).Dec()
	}
}
