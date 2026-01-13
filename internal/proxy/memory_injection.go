package proxy

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/eternisai/enchanted-proxy/internal/logger"
	"github.com/eternisai/enchanted-proxy/internal/memory"
)

// InjectMemoryIntoRequest adds user memory to the system prompt in the request body.
// If there's an existing system message, memory is appended to it.
// If there's no system message, a new one is created with just the memory.
// Returns the modified request body, or original if no memory or error.
func InjectMemoryIntoRequest(
	ctx context.Context,
	requestBody []byte,
	userID string,
	memoryService *memory.Service,
	log *logger.Logger,
) []byte {
	if memoryService == nil || userID == "" {
		return requestBody
	}

	// Fetch formatted memory
	formattedMemory, err := memoryService.GetFormattedMemory(ctx, userID)
	if err != nil {
		log.Error("failed to get formatted memory",
			"error", err.Error(),
			"user_id", userID,
		)
		return requestBody
	}

	if formattedMemory == "" {
		return requestBody
	}

	// Parse request body
	var reqBody map[string]interface{}
	if err := json.Unmarshal(requestBody, &reqBody); err != nil {
		log.Error("failed to parse request body for memory injection",
			"error", err.Error(),
		)
		return requestBody
	}

	// Get messages array
	messagesRaw, ok := reqBody["messages"]
	if !ok {
		log.Debug("no messages field in request body, skipping memory injection")
		return requestBody
	}

	messages, ok := messagesRaw.([]interface{})
	if !ok {
		log.Debug("messages field is not an array, skipping memory injection")
		return requestBody
	}

	// Find system message or create one
	systemMessageIndex := -1
	for i, msg := range messages {
		msgMap, ok := msg.(map[string]interface{})
		if !ok {
			continue
		}
		if role, ok := msgMap["role"].(string); ok && role == "system" {
			systemMessageIndex = i
			break
		}
	}

	if systemMessageIndex >= 0 {
		// Append memory to existing system message
		systemMsg := messages[systemMessageIndex].(map[string]interface{})
		existingContent, _ := systemMsg["content"].(string)

		// Append memory with separator
		var newContent string
		if strings.TrimSpace(existingContent) != "" {
			newContent = existingContent + "\n\n" + formattedMemory
		} else {
			newContent = formattedMemory
		}

		systemMsg["content"] = newContent
		messages[systemMessageIndex] = systemMsg

		log.Info("injected memory into existing system message",
			"user_id", userID,
			"memory_length", len(formattedMemory),
		)
	} else {
		// Create new system message with memory at the beginning
		systemMessage := map[string]interface{}{
			"role":    "system",
			"content": formattedMemory,
		}
		messages = append([]interface{}{systemMessage}, messages...)

		log.Info("created new system message with memory",
			"user_id", userID,
			"memory_length", len(formattedMemory),
		)
	}

	// Update messages in request body
	reqBody["messages"] = messages

	// Re-serialize request body
	modifiedBody, err := json.Marshal(reqBody)
	if err != nil {
		log.Error("failed to serialize modified request body",
			"error", err.Error(),
		)
		return requestBody
	}

	return modifiedBody
}
