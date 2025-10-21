package proxy

import "encoding/json"

// isFirstUserMessage checks if this is the first user message in a chat
// Returns true and the message content if exactly one user message exists (may have system/assistant messages)
func isFirstUserMessage(requestBody []byte) (bool, string) {
	var parsed struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}

	if err := json.Unmarshal(requestBody, &parsed); err != nil {
		return false, ""
	}

	// Count user messages and get the first one
	var userMessages []string
	for _, msg := range parsed.Messages {
		if msg.Role == "user" {
			userMessages = append(userMessages, msg.Content)
		}
	}

	// Return true only if there's exactly one user message
	if len(userMessages) == 1 {
		return true, userMessages[0]
	}

	return false, ""
}
