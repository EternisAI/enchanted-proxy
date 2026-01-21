package proxy

import (
	"context"

	"github.com/eternisai/enchanted-proxy/internal/title_generation"
	"github.com/gin-gonic/gin"
)

// TitleGenerationParams contains parameters for title generation
type TitleGenerationParams struct {
	UserID            string
	ChatID            string
	Model             string
	BaseURL           string
	APIKey            string
	Platform          string
	EncryptionEnabled *bool
}

// TriggerTitleGeneration checks if title generation should be triggered and handles it
func TriggerTitleGeneration(
	c *gin.Context,
	titleService *title_generation.Service,
	requestBody []byte,
	params TitleGenerationParams,
) {
	if titleService == nil || len(requestBody) == 0 {
		return
	}

	if params.UserID == "" || params.ChatID == "" {
		return
	}

	// Check for first message
	if isFirst, firstMessage := IsFirstUserMessage(requestBody); isFirst {
		go titleService.GenerateAndStore(
			context.Background(),
			title_generation.GenerateRequest{
				Model:       params.Model,
				BaseURL:     params.BaseURL,
				APIKey:      params.APIKey,
				UserContent: firstMessage,
			},
			title_generation.StorageRequest{
				UserID:            params.UserID,
				ChatID:            params.ChatID,
				Platform:          params.Platform,
				EncryptionEnabled: params.EncryptionEnabled,
			},
		)
		return
	}

	// Check for second message
	if isSecond, convCtx := IsSecondUserMessage(requestBody); isSecond {
		go titleService.RegenerateAndStore(
			context.Background(),
			title_generation.GenerateRequest{
				Model:   params.Model,
				BaseURL: params.BaseURL,
				APIKey:  params.APIKey,
			},
			title_generation.RegenerationContext{
				FirstUserMessage:  convCtx.FirstUserMessage,
				FirstAIResponse:   convCtx.FirstAIResponse,
				SecondUserMessage: convCtx.SecondUserMessage,
			},
			title_generation.StorageRequest{
				UserID:            params.UserID,
				ChatID:            params.ChatID,
				Platform:          params.Platform,
				EncryptionEnabled: params.EncryptionEnabled,
			},
		)
	}
}

// GetEncryptionEnabled extracts the encryption flag from gin context
func GetEncryptionEnabled(c *gin.Context) *bool {
	if val, exists := c.Get("encryptionEnabled"); exists {
		if boolPtr, ok := val.(*bool); ok {
			return boolPtr
		}
	}
	return nil
}
