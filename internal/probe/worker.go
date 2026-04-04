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

	"github.com/eternisai/enchanted-proxy/internal/config"
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
	slack    *slackNotifier
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

	// Token usage (nil if not available or non-2xx).
	usage *probeTokenUsage

	// Error response body (on non-2xx, truncated).
	body string
}

// maxJitterFraction is the maximum jitter as a fraction of the probe interval,
// applied to the initial delay to spread out probe workers.
const maxJitterFraction = 0.25

// maxJitterDelay caps the absolute jitter so long intervals (e.g. 15m) don't
// cause workers to sit idle for minutes before the first probe.
const maxJitterDelay = 1 * time.Minute

func (w *probeWorker) run() {
	defer w.service.wg.Done()

	// Apply random initial jitter (up to 25% of interval, capped at 1 minute)
	// to spread out workers and avoid synchronized bursts.
	jitterBase := min(time.Duration(float64(w.probe.Interval)*maxJitterFraction), maxJitterDelay)
	var jitter time.Duration
	if jitterBase > 0 {
		jitter = time.Duration(rand.Int64N(int64(jitterBase)))
	}

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
		// Only notify Slack on initial failure, not initial success (too noisy at startup).
		if !result.success {
			w.sendSlackNotification(result)
		}
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
				if w.ctx.Err() == nil {
					w.logStateChange(result)
					w.sendSlackNotification(result)
				}
			} else if !result.success && healthy {
				healthy = false
				ticker.Reset(w.probe.RetryInterval)
				// Don't log shutdown-induced failures as state changes.
				if w.ctx.Err() == nil {
					w.logStateChange(result)
					w.sendSlackNotification(result)
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
		if result.usage != nil {
			attrs = append(attrs,
				slog.Int("prompt_tokens", result.usage.PromptTokens),
				slog.Int("completion_tokens", result.usage.CompletionTokens))
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

// sendSlackNotification sends a Slack notification for a probe state change, if configured.
func (w *probeWorker) sendSlackNotification(result probeResult) {
	if w.slack == nil {
		return
	}
	ctx, cancel := context.WithTimeout(w.ctx, 10*time.Second)
	defer cancel()
	if err := w.slack.sendProbeNotification(ctx, w.provider, w.model, result); err != nil {
		w.logger.Warn("failed to send slack notification",
			slog.String("provider", w.provider),
			slog.String("model", w.model),
			slog.String("error", err.Error()))
	}
}

// runProbe executes a single probe request and returns the result.
// Only logs at Error level for programming errors (marshal/request creation failures).
// State-transition logging is handled by the caller (run method).
func (w *probeWorker) runProbe() probeResult {
	var reqBody map[string]any
	if w.endpoint.APIType == config.APITypeResponses {
		reqBody = buildResponsesProbeRequestBody(w.endpoint, w.probe)
	} else {
		reqBody = buildProbeRequestBody(w.endpoint, w.probe)
	}

	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		w.logger.Error("failed to marshal probe request",
			slog.String("provider", w.provider),
			slog.String("model", w.model),
			slog.String("error", err.Error()))
		return probeResult{err: err}
	}

	var url string
	if w.endpoint.APIType == config.APITypeResponses {
		url = strings.TrimRight(w.endpoint.BaseURL, "/") + "/responses"
	} else {
		url = strings.TrimRight(w.endpoint.BaseURL, "/") + "/chat/completions"
	}
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
		w.logger.Debug("probe request failed",
			slog.String("provider", w.provider),
			slog.String("model", w.model),
			slog.Duration("duration", duration),
			slog.String("error", err.Error()))
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
	var parsed parsedResponse
	if w.endpoint.APIType == config.APITypeResponses {
		parsed = parseResponsesAPIResponse(respBody)
	} else {
		parsed = parseResponse(respBody)
	}

	// Check content match if configured and response was successful.
	// Content checking is only active when ExpectedResponse is non-nil AND non-empty.
	// Strip thinking tags (e.g. DeepSeek R1 wraps reasoning in <think>...</think>),
	// trim whitespace, then do a case-insensitive exact match.
	contentMatch := false
	hasExpectedResponse := w.probe.ExpectedResponse != nil && *w.probe.ExpectedResponse != ""

	if resp.StatusCode >= 200 && resp.StatusCode < 300 && hasExpectedResponse {
		content := parsed.content
		if idx := strings.LastIndex(content, "</think>"); idx != -1 {
			content = content[idx+len("</think>"):]
		}
		contentMatch = strings.EqualFold(
			strings.TrimSpace(content),
			strings.TrimSpace(*w.probe.ExpectedResponse),
		)
	}

	recordProbeResult(w.provider, w.model, resp.StatusCode, duration.Seconds(), contentMatch, hasExpectedResponse, parsed.usage)

	success := resp.StatusCode >= 200 && resp.StatusCode < 300
	if success && hasExpectedResponse && !contentMatch {
		success = false
	}

	result := probeResult{
		success:    success,
		statusCode: resp.StatusCode,
		duration:   duration,
		usage:      parsed.usage,
	}

	if resp.StatusCode >= 200 && resp.StatusCode < 300 && hasExpectedResponse && !contentMatch {
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

// responsesAPIResponse is a minimal representation of an OpenAI Responses API response.
// See: https://platform.openai.com/docs/api-reference/responses
type responsesAPIResponse struct {
	Status string `json:"status"`
	Output []struct {
		Type    string `json:"type"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	} `json:"output"`
	Usage *struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage,omitempty"`
}

// parsedResponse holds the extracted fields from a probe response (any API type).
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

// parseResponsesAPIResponse extracts content and token usage from a Responses API response.
// Content is extracted from the first output_text content block in the first message output.
func parseResponsesAPIResponse(body []byte) parsedResponse {
	var resp responsesAPIResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return parsedResponse{}
	}

	var result parsedResponse
	for _, output := range resp.Output {
		if output.Type != "message" {
			continue
		}
		for _, content := range output.Content {
			if content.Type == "output_text" {
				result.content = content.Text
				break
			}
		}
		if result.content != "" {
			break
		}
	}
	if resp.Usage != nil {
		result.usage = &probeTokenUsage{
			PromptTokens:     resp.Usage.InputTokens,
			CompletionTokens: resp.Usage.OutputTokens,
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
