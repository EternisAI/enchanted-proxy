package proxy

import (
	"encoding/json"
	"strings"

	"github.com/eternisai/enchanted-proxy/internal/auth"
	"github.com/eternisai/enchanted-proxy/internal/messaging"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// extractContentFromSSELine extracts content delta from SSE line
func extractContentFromSSELine(line string) string {
	if !strings.HasPrefix(line, "data: ") {
		return ""
	}

	data := strings.TrimPrefix(line, "data: ")
	if data == "[DONE]" {
		return ""
	}

	var chunk map[string]interface{}
	if err := json.Unmarshal([]byte(data), &chunk); err != nil {
		return ""
	}

	choices, ok := chunk["choices"].([]interface{})
	if !ok || len(choices) == 0 {
		return ""
	}

	firstChoice, ok := choices[0].(map[string]interface{})
	if !ok {
		return ""
	}

	delta, ok := firstChoice["delta"].(map[string]interface{})
	if !ok {
		return ""
	}

	content, ok := delta["content"].(string)
	if !ok {
		return ""
	}

	return content
}

// extractContentFromResponse extracts content from non-streaming response
func extractContentFromResponse(responseBody []byte) string {
	var parsed struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}

	if err := json.Unmarshal(responseBody, &parsed); err != nil {
		return ""
	}

	if len(parsed.Choices) == 0 {
		return ""
	}

	return parsed.Choices[0].Message.Content
}

// saveMessageAsync saves a message to Firestore asynchronously
func saveMessageAsync(c *gin.Context, messageService *messaging.Service, content string, isError bool) {
	if messageService == nil {
		return
	}

	// Skip if content is empty
	if strings.TrimSpace(content) == "" {
		return
	}

	// Extract user ID
	userID, exists := auth.GetUserUUID(c)
	if !exists {
		return
	}

	// Extract or generate chat ID
	chatID := c.GetHeader("X-Chat-ID")
	if chatID == "" {
		// Fallback: generate a new chat ID (though ideally client should provide)
		chatID = uuid.New().String()
	}

	// Build message (assistant response)
	msg := messaging.MessageToStore{
		UserID:     userID,
		ChatID:     chatID,
		MessageID:  uuid.New().String(),
		IsFromUser: false, // This is an assistant response
		Content:    content,
		IsError:    isError,
	}

	// Store asynchronously using background context
	// Service applies its own timeout, don't use request context which gets cancelled when handler returns
	if err := messageService.StoreMessageAsync(context.Background(), msg); err != nil {
		// Log error but don't fail the request
		// The error is already logged within the service
	}
}
