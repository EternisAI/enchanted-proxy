package probe

import (
	"strings"

	"github.com/eternisai/enchanted-proxy/internal/routing"
)

// buildProbeRequestBody constructs the JSON body for a probe chat completion request.
func buildProbeRequestBody(endpoint *routing.ProviderConfig, probe *routing.ProbeConfig) map[string]any {
	reasoning := isOpenAIReasoningModel(endpoint.Model)

	req := map[string]any{
		"model": endpoint.Model,
		"messages": []map[string]string{
			{"role": "user", "content": probe.Prompt},
		},
		"stream": false,
	}

	// OpenAI reasoning models (o-series) require max_completion_tokens and
	// don't support temperature. Standard models use max_tokens + temperature.
	if reasoning {
		req["max_completion_tokens"] = probe.MaxTokens
	} else {
		req["max_tokens"] = probe.MaxTokens
		req["temperature"] = probe.Temperature
	}

	// Apply thinking suppression when thinking is disabled (default).
	// Only apply for models where we know a reliable API parameter.
	if !probe.Thinking && reasoning {
		req["reasoning_effort"] = "low"
	}

	return req
}

// buildResponsesProbeRequestBody constructs the JSON body for a probe using the
// OpenAI Responses API. Uses synchronous mode (no background/store) since we just
// need a quick health check, not a persistent conversation.
func buildResponsesProbeRequestBody(endpoint *routing.ProviderConfig, probe *routing.ProbeConfig) map[string]any {
	req := map[string]any{
		"model": endpoint.Model,
		"input": []map[string]string{
			{"role": "user", "content": probe.Prompt},
		},
		"max_output_tokens": probe.MaxTokens,
		"store":             false,
	}

	// Suppress reasoning to save tokens during health checks.
	// Use "medium" (not "low") because pro models only support medium/high/xhigh.
	if !probe.Thinking {
		req["reasoning"] = map[string]any{"effort": "medium"}
	}

	return req
}

// isOpenAIReasoningModel checks if the model name matches known OpenAI reasoning models
// that support the reasoning_effort parameter. It requires a delimiter ("-") after
// the series prefix to avoid false-positives on unrelated models (e.g. "ollama-xyz").
func isOpenAIReasoningModel(model string) bool {
	lower := strings.ToLower(model)
	for _, prefix := range []string{"o1-", "o3-", "o4-"} {
		if strings.HasPrefix(lower, prefix) {
			return true
		}
	}
	return false
}
