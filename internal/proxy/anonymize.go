package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/eternisai/enchanted-proxy/internal/anonymizer"
	"github.com/eternisai/enchanted-proxy/internal/logger"
	"github.com/eternisai/enchanted-proxy/internal/streaming"
)

// anonymizeRequestBody runs the last user message through the anonymizer and returns
// the modified request body with the anonymized message, plus the JSON-encoded replacements.
// Returns (modifiedBody, replacementsJSON, ok). On failure, logs a warning and returns ok=false
// so the caller can proceed with the original body (graceful degradation).
func anonymizeRequestBody(ctx context.Context, log *logger.Logger, svc *anonymizer.Service, requestBody []byte) ([]byte, string, bool) {
	// Extract last user message
	userMessage := extractLastUserMessage(requestBody)
	if userMessage == "" {
		log.Debug("anonymizer: no user message found in request body")
		return nil, "", false
	}

	result, err := svc.Anonymize(ctx, userMessage)
	if err != nil {
		log.Warn("anonymizer: call failed, proceeding without anonymization",
			slog.String("error", err.Error()))
		return nil, "", false
	}

	if len(result.Replacements) == 0 {
		log.Debug("anonymizer: no PII detected")
		return nil, "", false
	}

	// Serialize replacements for the response header
	replacementsJSON, err := json.Marshal(result.Replacements)
	if err != nil {
		log.Warn("anonymizer: failed to marshal replacements",
			slog.String("error", err.Error()))
		return nil, "", false
	}

	// Replace the last user message in the request body with the anonymized version
	modifiedBody, err := replaceLastUserMessage(requestBody, result.Text)
	if err != nil {
		log.Warn("anonymizer: failed to replace user message in request body",
			slog.String("error", err.Error()))
		return nil, "", false
	}

	log.Info("anonymizer: message anonymized",
		slog.Int("replacements", len(result.Replacements)))

	return modifiedBody, string(replacementsJSON), true
}

// replaceLastUserMessage replaces the content of the last user message in the
// OpenAI-compatible request body with the given text.
func replaceLastUserMessage(requestBody []byte, newContent string) ([]byte, error) {
	var reqBody map[string]interface{}
	if err := json.Unmarshal(requestBody, &reqBody); err != nil {
		return nil, err
	}

	messages, ok := reqBody["messages"].([]interface{})
	if !ok || len(messages) == 0 {
		return nil, fmt.Errorf("no messages in request body")
	}

	// Find and replace the last user message
	for i := len(messages) - 1; i >= 0; i-- {
		msg, ok := messages[i].(map[string]interface{})
		if !ok {
			continue
		}
		if role, _ := msg["role"].(string); role == "user" {
			msg["content"] = newContent
			break
		}
	}

	return json.Marshal(reqBody)
}

// deanonymizeResponseBody reverses anonymized tokens in the content field of a
// non-streaming OpenAI-compatible response body. Returns the modified JSON, or nil if
// no changes were needed.
func deanonymizeResponseBody(body []byte, d *streaming.Deanonymizer) []byte {
	var parsed map[string]interface{}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil
	}

	choices, ok := parsed["choices"].([]interface{})
	if !ok || len(choices) == 0 {
		return nil
	}

	changed := false
	for _, choice := range choices {
		choiceMap, ok := choice.(map[string]interface{})
		if !ok {
			continue
		}
		msg, ok := choiceMap["message"].(map[string]interface{})
		if !ok {
			continue
		}
		content, ok := msg["content"].(string)
		if !ok || content == "" {
			continue
		}
		replaced := d.ReplaceInText(content)
		if replaced != content {
			msg["content"] = replaced
			changed = true
		}
	}

	if !changed {
		return nil
	}

	modified, err := json.Marshal(parsed)
	if err != nil {
		return nil
	}
	return modified
}
