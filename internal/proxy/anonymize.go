package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/eternisai/enchanted-proxy/internal/anonymizer"
	"github.com/eternisai/enchanted-proxy/internal/logger"
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
