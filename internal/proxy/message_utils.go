package proxy

import (
	"context"
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

// extractLastUserMessage extracts the last user message from the request body
func extractLastUserMessage(requestBody []byte) string {
	var parsed struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}

	if err := json.Unmarshal(requestBody, &parsed); err != nil {
		return ""
	}

	// Find last user message
	for i := len(parsed.Messages) - 1; i >= 0; i-- {
		if parsed.Messages[i].Role == "user" {
			return parsed.Messages[i].Content
		}
	}

	return ""
}

// saveUserMessageAsync saves a user message to Firestore asynchronously
// Follows STREAMING_BROADCAST_PLAN.md Section 3 (Server-Side User Message Storage)
//
// Required headers for proxy-side storage:
//   - X-Chat-ID: Chat session ID
//   - X-User-Message-ID: User message ID (unique identifier)
//
// Optional headers:
//   - X-Encryption-Enabled: "true" to encrypt message with user's public key
//
// Backward Compatibility:
//   - If X-User-Message-ID is MISSING: proxy does NOT save user message
//     → Old clients continue saving to Firestore themselves (no duplicate)
//   - If X-User-Message-ID is PRESENT: proxy saves user message
//     → New clients can remove their own Firestore writes
//
// Behavior:
//   - Extracts last user message from request body
//   - Stores to Firestore: users/{userId}/chats/{chatId}/messages/{messageId}
//   - Uses async worker pool (non-blocking)
//   - Encryption: fetches public key from Firestore if enabled
func saveUserMessageAsync(c *gin.Context, messageService *messaging.Service, requestBody []byte) {
	if messageService == nil {
		return
	}

	// BACKWARD COMPATIBILITY: Only save if X-User-Message-ID is provided
	// This prevents double-saving when old clients already write to Firestore themselves
	messageID := c.GetHeader("X-User-Message-ID")
	if messageID == "" {
		// Old client behavior: they save user message themselves
		// Skip proxy-side storage to avoid duplicates
		return
	}

	// Skip if X-Chat-ID header not provided
	// This prevents creating orphan chats for non-chat requests
	chatID := c.GetHeader("X-Chat-ID")
	if chatID == "" {
		return
	}

	// Extract user message content
	content := extractLastUserMessage(requestBody)
	if strings.TrimSpace(content) == "" {
		return
	}

	// Extract user ID (Firebase UID for Firestore paths)
	userID, exists := auth.GetUserID(c)
	if !exists {
		return
	}

	// Extract encryption enabled flag from context (set by ProxyHandler)
	var encryptionEnabled *bool
	if val, exists := c.Get("encryptionEnabled"); exists {
		if boolPtr, ok := val.(*bool); ok {
			encryptionEnabled = boolPtr
		}
	}

	// Extract fallback flag
	isFallbackRequest := false
	if val, exists := c.Get("isFallbackRequest"); exists {
		if boolVal, ok := val.(bool); ok {
			isFallbackRequest = boolVal
		}
	}

	// Build message (user message)
	msg := messaging.MessageToStore{
		UserID:            userID,
		ChatID:            chatID,
		MessageID:         messageID,
		IsFromUser:        true, // This is a user message
		Content:           content,
		IsError:           false,
		EncryptionEnabled: encryptionEnabled,
		FallbackModeUsed:  isFallbackRequest,
	}

	// Store asynchronously using background context
	// Service applies its own timeout, don't use request context which gets cancelled when handler returns
	if err := messageService.StoreMessageAsync(context.Background(), msg); err != nil {
		// Log error but don't fail the request
		// The error is already logged within the service
	}
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

	// Skip if X-Chat-ID header not provided
	// This prevents creating orphan chats for non-chat requests (e.g., title generation)
	chatID := c.GetHeader("X-Chat-ID")
	if chatID == "" {
		return
	}

	// Extract user ID (Firebase UID for Firestore paths)
	userID, exists := auth.GetUserID(c)
	if !exists {
		return
	}

	// Extract or generate message ID
	messageID := c.GetHeader("X-Message-ID")
	if messageID == "" {
		// Fallback: generate a new message ID if client doesn't provide one
		messageID = uuid.New().String()
	}

	// Extract encryption enabled flag from context (set by ProxyHandler)
	var encryptionEnabled *bool
	if val, exists := c.Get("encryptionEnabled"); exists {
		if boolPtr, ok := val.(*bool); ok {
			encryptionEnabled = boolPtr
		}
	}

	// Extract fallback flag
	isFallbackRequest := false
	if val, exists := c.Get("isFallbackRequest"); exists {
		if boolVal, ok := val.(bool); ok {
			isFallbackRequest = boolVal
		}
	}

	// Build message (assistant response)
	msg := messaging.MessageToStore{
		UserID:            userID,
		ChatID:            chatID,
		MessageID:         messageID,
		IsFromUser:        false, // This is an assistant response
		Content:           content,
		IsError:           isError,
		EncryptionEnabled: encryptionEnabled,
		FallbackModeUsed:  isFallbackRequest,
	}

	// Store asynchronously using background context
	// Service applies its own timeout, don't use request context which gets cancelled when handler returns
	if err := messageService.StoreMessageAsync(context.Background(), msg); err != nil {
		// Log error but don't fail the request
		// The error is already logged within the service
	}
}
