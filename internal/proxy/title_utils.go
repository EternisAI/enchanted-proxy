package proxy

import "encoding/json"

// ConversationContext holds extracted conversation messages for title regeneration
type ConversationContext struct {
	FirstUserMessage  string
	FirstAIResponse   string
	SecondUserMessage string
}

// countUserMessages parses the request body and returns message counts
func countUserMessages(requestBody []byte) (userMsgs []string, assistantMsgs []string, ok bool) {
	var parsed struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}

	if err := json.Unmarshal(requestBody, &parsed); err != nil {
		return nil, nil, false
	}

	for _, msg := range parsed.Messages {
		switch msg.Role {
		case "user":
			userMsgs = append(userMsgs, msg.Content)
		case "assistant":
			assistantMsgs = append(assistantMsgs, msg.Content)
		}
	}

	return userMsgs, assistantMsgs, true
}

// IsFirstUserMessage checks if this is the first user message in a chat
// Returns true and the message content if exactly one user message exists
func IsFirstUserMessage(requestBody []byte) (bool, string) {
	userMsgs, _, ok := countUserMessages(requestBody)
	if !ok {
		return false, ""
	}

	if len(userMsgs) == 1 {
		return true, userMsgs[0]
	}

	return false, ""
}

// IsSecondUserMessage checks if this is the second user message in a chat
// Returns true and the conversation context for title regeneration
func IsSecondUserMessage(requestBody []byte) (bool, ConversationContext) {
	userMsgs, assistantMsgs, ok := countUserMessages(requestBody)
	if !ok {
		return false, ConversationContext{}
	}

	if len(userMsgs) == 2 && len(assistantMsgs) >= 1 {
		return true, ConversationContext{
			FirstUserMessage:  userMsgs[0],
			FirstAIResponse:   assistantMsgs[0],
			SecondUserMessage: userMsgs[1],
		}
	}

	return false, ConversationContext{}
}
