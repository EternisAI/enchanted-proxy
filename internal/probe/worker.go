package probe

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"strings"
	"time"

	"github.com/eternisai/enchanted-proxy/internal/logger"
	"github.com/eternisai/enchanted-proxy/internal/routing"
)

const (
	probeHTTPTimeout = 30 * time.Second
	maxResponseBytes = 4096 // limit response body read to avoid unbounded memory
)

type probeWorker struct {
	service  *ProbeService
	ctx      context.Context // cancelled on shutdown to abort in-flight requests
	provider string          // provider name (for metrics labels and logging)
	model    string          // canonical model name (for metrics labels and logging)
	endpoint *routing.ProviderConfig
	probe    *routing.ProbeConfig
	client   *http.Client
	logger   *logger.Logger
}

// probeResult holds the outcome of a single probe execution.
type probeResult struct {
	success    bool
	statusCode int
	duration   time.Duration
	err        error // set on connection/read errors

	// Content match info (only meaningful on 2xx with expected response configured).
	contentMismatch bool
	expected        string
	got             string

	// Error response body (on non-2xx, truncated).
	body string
}

// maxJitterFraction is the maximum jitter as a fraction of the probe interval,
// applied to the initial delay to spread out probe workers.
const maxJitterFraction = 0.25

func (w *probeWorker) run() {
	defer w.service.wg.Done()

	// Apply random initial jitter (up to 25% of interval) to spread out workers
	// that share the same interval and avoid synchronized bursts.
	jitter := time.Duration(rand.Int64N(int64(float64(w.probe.Interval) * maxJitterFraction)))

	w.logger.Debug("started probe worker",
		slog.String("provider", w.provider),
		slog.String("model", w.model),
		slog.Duration("interval", w.probe.Interval),
		slog.Duration("retry_interval", w.probe.RetryInterval),
		slog.Duration("initial_jitter", jitter))

	// Wait for jitter before the first probe, respecting shutdown.
	var result probeResult
	select {
	case <-time.After(jitter):
		result = w.runProbe()
	case <-w.service.shutdown:
		return
	}

	healthy := result.success
	// Log initial state unless we're shutting down.
	if w.ctx.Err() == nil {
		w.logStateChange(result)
	}

	currentInterval := w.probe.Interval
	if !healthy {
		currentInterval = w.probe.RetryInterval
	}

	ticker := time.NewTicker(currentInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			result = w.runProbe()
			if result.success && !healthy {
				healthy = true
				ticker.Reset(w.probe.Interval)
				w.logStateChange(result)
			} else if !result.success && healthy {
				healthy = false
				ticker.Reset(w.probe.RetryInterval)
				// Don't log shutdown-induced failures as state changes.
				if w.ctx.Err() == nil {
					w.logStateChange(result)
				}
			}
		case <-w.service.shutdown:
			w.logger.Debug("stopped probe worker",
				slog.String("provider", w.provider),
				slog.String("model", w.model))
			return
		}
	}
}

// logStateChange logs a probe state transition (initial state, recovery, or new failure).
func (w *probeWorker) logStateChange(result probeResult) {
	if result.success {
		attrs := []any{
			slog.String("provider", w.provider),
			slog.String("model", w.model),
			slog.Int("status", result.statusCode),
			slog.Duration("duration", result.duration),
		}
		if result.contentMismatch {
			attrs = append(attrs,
				slog.String("expected_content", result.expected),
				slog.String("actual_content", result.got))
		}
		w.logger.Info("probe succeeded", attrs...)
	} else {
		attrs := []any{
			slog.String("provider", w.provider),
			slog.String("model", w.model),
		}
		if result.err != nil {
			attrs = append(attrs,
				slog.Duration("duration", result.duration),
				slog.String("error", result.err.Error()))
		} else {
			attrs = append(attrs,
				slog.Int("status", result.statusCode),
				slog.Duration("duration", result.duration),
				slog.String("body", result.body))
		}
		w.logger.Warn("probe failed", attrs...)
	}
}

// runProbe executes a single probe request and returns the result.
// Only logs at Error level for programming errors (marshal/request creation failures).
// State-transition logging is handled by the caller (run method).
func (w *probeWorker) runProbe() probeResult {
	reqBody := buildProbeRequestBody(w.endpoint, w.probe)

	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		w.logger.Error("failed to marshal probe request",
			slog.String("provider", w.provider),
			slog.String("model", w.model),
			slog.String("error", err.Error()))
		return probeResult{err: err}
	}

	url := strings.TrimRight(w.endpoint.BaseURL, "/") + "/chat/completions"
	req, err := http.NewRequestWithContext(w.ctx, "POST", url, bytes.NewReader(bodyBytes))
	if err != nil {
		w.logger.Error("failed to create probe request",
			slog.String("provider", w.provider),
			slog.String("model", w.model),
			slog.String("error", err.Error()))
		return probeResult{err: err}
	}

	req.Header.Set("Content-Type", "application/json")
	if w.endpoint.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+w.endpoint.APIKey)
	}

	start := time.Now()
	resp, err := w.client.Do(req)
	duration := time.Since(start)

	if err != nil {
		// Don't record metrics for shutdown-induced cancellations.
		if w.ctx.Err() != nil {
			return probeResult{err: err}
		}
		recordProbeResult(w.provider, w.model, 0, duration.Seconds(), false, false, nil)
		return probeResult{duration: duration, err: err}
	}
	defer resp.Body.Close()

	// Read response body (limited).
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		recordProbeResult(w.provider, w.model, resp.StatusCode, duration.Seconds(), false, false, nil)
		return probeResult{
			statusCode: resp.StatusCode,
			duration:   duration,
			err:        fmt.Errorf("reading response body: %w", err),
		}
	}

	// Parse the response to extract content and token usage.
	parsed := parseResponse(respBody)

	// Check content match if configured and response was successful.
	// Content checking is only active when ExpectedResponse is non-nil AND non-empty.
	// Trim whitespace from response content to handle thinking models that pad output.
	contentMatch := false
	hasExpectedResponse := w.probe.ExpectedResponse != nil && *w.probe.ExpectedResponse != ""

	if resp.StatusCode >= 200 && resp.StatusCode < 300 && hasExpectedResponse {
		contentMatch = strings.Contains(
			strings.ToLower(strings.TrimSpace(parsed.content)),
			strings.ToLower(*w.probe.ExpectedResponse),
		)
	}

	recordProbeResult(w.provider, w.model, resp.StatusCode, duration.Seconds(), contentMatch, hasExpectedResponse, parsed.usage)

	success := resp.StatusCode >= 200 && resp.StatusCode < 300

	result := probeResult{
		success:    success,
		statusCode: resp.StatusCode,
		duration:   duration,
	}

	if success && hasExpectedResponse && !contentMatch {
		result.contentMismatch = true
		result.expected = *w.probe.ExpectedResponse
		result.got = truncate(parsed.content, 100)
	}

	if !success {
		result.body = truncate(string(respBody), 200)
	}

	return result
}

// chatCompletionResponse is a minimal representation of the OpenAI chat completion response.
type chatCompletionResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage,omitempty"`
}

// parsedResponse holds the extracted fields from a chat completion response.
type parsedResponse struct {
	content string
	usage   *probeTokenUsage
}

// parseResponse extracts message content and token usage from a chat completion response body.
func parseResponse(body []byte) parsedResponse {
	var resp chatCompletionResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return parsedResponse{}
	}

	var result parsedResponse
	if len(resp.Choices) > 0 {
		result.content = resp.Choices[0].Message.Content
	}
	if resp.Usage != nil {
		result.usage = &probeTokenUsage{
			PromptTokens:     resp.Usage.PromptTokens,
			CompletionTokens: resp.Usage.CompletionTokens,
		}
	}
	return result
}

// truncate shortens a string to maxLen runes, appending "..." if truncated.
func truncate(s string, maxLen int) string {
	runes := []rune(s)
	if len(runes) <= maxLen {
		return s
	}
	return string(runes[:maxLen]) + fmt.Sprintf("... (%d bytes total)", len(s))
}
