package probe

import (
	"strings"

	"github.com/eternisai/enchanted-proxy/internal/routing"
)

// buildProbeRequestBody constructs the JSON body for a probe chat completion request.
func buildProbeRequestBody(endpoint *routing.ProviderConfig, probe *routing.ProbeConfig) map[string]any {
	req := map[string]any{
		"model": endpoint.Model,
		"messages": []map[string]string{
			{"role": "user", "content": probe.Prompt},
		},
		"max_tokens":  probe.MaxTokens,
		"temperature": probe.Temperature,
		"stream":      false,
	}

	// Apply thinking suppression when thinking is disabled (default).
	// Only apply for models where we know a reliable API parameter.
	if !probe.Thinking {
		if isOpenAIReasoningModel(endpoint.Model) {
			req["reasoning_effort"] = "low"
		}
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
	if !probe.Thinking {
		req["reasoning"] = map[string]any{"effort": "low"}
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
