package probe

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/eternisai/enchanted-proxy/internal/logger"
	"github.com/eternisai/enchanted-proxy/internal/metrics"
	"github.com/eternisai/enchanted-proxy/internal/routing"
)

const (
	probeHTTPTimeout = 30 * time.Second
	maxResponseBytes = 4096 // limit response body read to avoid unbounded memory
)

type probeWorker struct {
	service  *ProbeService
	provider string // provider name (for metrics labels and logging)
	model    string // canonical model name (for metrics labels and logging)
	endpoint *routing.ProviderConfig
	probe    *routing.ProbeConfig
	client   *http.Client
	logger   *logger.Logger
}

func (w *probeWorker) run() {
	defer w.service.wg.Done()

	w.logger.Debug("started probe worker",
		slog.String("provider", w.provider),
		slog.String("model", w.model),
		slog.Duration("interval", w.probe.Interval))

	// Run first probe immediately, then on ticker interval.
	w.runProbe()

	ticker := time.NewTicker(w.probe.Interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			w.runProbe()
		case <-w.service.shutdown:
			w.logger.Debug("stopped probe worker",
				slog.String("provider", w.provider),
				slog.String("model", w.model))
			return
		}
	}
}

func (w *probeWorker) runProbe() {
	reqBody := buildProbeRequestBody(w.endpoint, w.probe)

	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		w.logger.Error("failed to marshal probe request",
			slog.String("provider", w.provider),
			slog.String("model", w.model),
			slog.String("error", err.Error()))
		return
	}

	url := w.endpoint.BaseURL + "/chat/completions"
	req, err := http.NewRequest("POST", url, bytes.NewReader(bodyBytes))
	if err != nil {
		w.logger.Error("failed to create probe request",
			slog.String("provider", w.provider),
			slog.String("model", w.model),
			slog.String("error", err.Error()))
		return
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+w.endpoint.APIKey)

	start := time.Now()
	resp, err := w.client.Do(req)
	duration := time.Since(start)

	if err != nil {
		metrics.RecordProbeResult(w.provider, w.model, 0, duration.Seconds(), false, w.probe.ExpectedResponse != nil)
		w.logger.Warn("probe request failed",
			slog.String("provider", w.provider),
			slog.String("model", w.model),
			slog.Duration("duration", duration),
			slog.String("error", err.Error()))
		return
	}
	defer resp.Body.Close()

	// Read response body (limited).
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		metrics.RecordProbeResult(w.provider, w.model, resp.StatusCode, duration.Seconds(), false, w.probe.ExpectedResponse != nil)
		w.logger.Warn("failed to read probe response body",
			slog.String("provider", w.provider),
			slog.String("model", w.model),
			slog.Int("status", resp.StatusCode),
			slog.Duration("duration", duration),
			slog.String("error", err.Error()))
		return
	}

	// Check content match if configured and response was successful.
	contentMatch := false
	hasExpectedResponse := w.probe.ExpectedResponse != nil

	if resp.StatusCode >= 200 && resp.StatusCode < 300 && hasExpectedResponse {
		content := extractResponseContent(respBody)
		contentMatch = strings.Contains(
			strings.ToLower(content),
			strings.ToLower(*w.probe.ExpectedResponse),
		)
	}

	metrics.RecordProbeResult(w.provider, w.model, resp.StatusCode, duration.Seconds(), contentMatch, hasExpectedResponse)

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		if hasExpectedResponse && !contentMatch {
			content := extractResponseContent(respBody)
			w.logger.Warn("probe succeeded but response content unexpected",
				slog.String("provider", w.provider),
				slog.String("model", w.model),
				slog.Int("status", resp.StatusCode),
				slog.Duration("duration", duration),
				slog.String("expected", *w.probe.ExpectedResponse),
				slog.String("got", truncate(content, 100)))
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
}

// chatCompletionResponse is a minimal representation of the OpenAI chat completion response
// used only to extract the first choice's message content.
type chatCompletionResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
}

// extractResponseContent extracts the message content from a chat completion response body.
func extractResponseContent(body []byte) string {
	var resp chatCompletionResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return ""
	}
	if len(resp.Choices) > 0 {
		return resp.Choices[0].Message.Content
	}
	return ""
}

// truncate shortens a string to maxLen, appending "..." if truncated.
func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + fmt.Sprintf("... (%d bytes total)", len(s))
}
