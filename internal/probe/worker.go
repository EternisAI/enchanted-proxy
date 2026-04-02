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
	var success bool
	select {
	case <-time.After(jitter):
		success = w.runProbe()
	case <-w.service.shutdown:
		return
	}

	healthy := success
	currentInterval := w.probe.Interval
	if !healthy {
		currentInterval = w.probe.RetryInterval
	}

	ticker := time.NewTicker(currentInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			success = w.runProbe()
			if success && !healthy {
				healthy = true
				ticker.Reset(w.probe.Interval)
				w.logger.Info("probe recovered, switching to normal interval",
					slog.String("provider", w.provider),
					slog.String("model", w.model),
					slog.Duration("interval", w.probe.Interval))
			} else if !success && healthy {
				healthy = false
				ticker.Reset(w.probe.RetryInterval)
				w.logger.Warn("probe failed, switching to retry interval",
					slog.String("provider", w.provider),
					slog.String("model", w.model),
					slog.Duration("retry_interval", w.probe.RetryInterval))
			}
		case <-w.service.shutdown:
			w.logger.Debug("stopped probe worker",
				slog.String("provider", w.provider),
				slog.String("model", w.model))
			return
		}
	}
}

// runProbe executes a single probe request. Returns true if the probe succeeded (2xx status).
func (w *probeWorker) runProbe() bool {
	reqBody := buildProbeRequestBody(w.endpoint, w.probe)

	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		w.logger.Error("failed to marshal probe request",
			slog.String("provider", w.provider),
			slog.String("model", w.model),
			slog.String("error", err.Error()))
		return false
	}

	url := strings.TrimRight(w.endpoint.BaseURL, "/") + "/chat/completions"
	req, err := http.NewRequestWithContext(w.ctx, "POST", url, bytes.NewReader(bodyBytes))
	if err != nil {
		w.logger.Error("failed to create probe request",
			slog.String("provider", w.provider),
			slog.String("model", w.model),
			slog.String("error", err.Error()))
		return false
	}

	req.Header.Set("Content-Type", "application/json")
	if w.endpoint.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+w.endpoint.APIKey)
	}

	start := time.Now()
	resp, err := w.client.Do(req)
	duration := time.Since(start)

	if err != nil {
		// Don't record metrics or log warnings for shutdown-induced cancellations.
		if w.ctx.Err() != nil {
			return false
		}
		recordProbeResult(w.provider, w.model, 0, duration.Seconds(), false, false, nil)
		w.logger.Warn("probe request failed",
			slog.String("provider", w.provider),
			slog.String("model", w.model),
			slog.Duration("duration", duration),
			slog.String("error", err.Error()))
		return false
	}
	defer resp.Body.Close()

	// Read response body (limited).
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		recordProbeResult(w.provider, w.model, resp.StatusCode, duration.Seconds(), false, false, nil)
		w.logger.Warn("failed to read probe response body",
			slog.String("provider", w.provider),
			slog.String("model", w.model),
			slog.Int("status", resp.StatusCode),
			slog.Duration("duration", duration),
			slog.String("error", err.Error()))
		return false
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

	if success {
		if hasExpectedResponse && !contentMatch {
			w.logger.Warn("probe succeeded but response content unexpected",
				slog.String("provider", w.provider),
				slog.String("model", w.model),
				slog.Int("status", resp.StatusCode),
				slog.Duration("duration", duration),
				slog.String("expected", *w.probe.ExpectedResponse),
				slog.String("got", truncate(parsed.content, 100)))
		} else {
			w.logger.Debug("probe succeeded",
				slog.String("provider", w.provider),
				slog.String("model", w.model),
				slog.Int("status", resp.StatusCode),
				slog.Duration("duration", duration))
		}
	} else {
		w.logger.Warn("probe returned non-2xx status",
			slog.String("provider", w.provider),
			slog.String("model", w.model),
			slog.Int("status", resp.StatusCode),
			slog.Duration("duration", duration),
			slog.String("body", truncate(string(respBody), 200)))
	}

	return success
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
