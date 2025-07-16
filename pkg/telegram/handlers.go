package telegram

import (
	"context"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// Handler handles HTTP requests for Telegram operations
type Handler struct {
	service *Service
}

// NewHandler creates a new Telegram handler instance
func NewHandler(service *Service) *Handler {
	return &Handler{
		service: service,
	}
}

// SendMessage handles sending a message to a Telegram chat
// POST /telegram/send
func (h *Handler) SendMessage(c *gin.Context) {
	var req SendMessageRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error":   "Invalid request format",
			"details": err.Error(),
		})
		return
	}

	// Validate required fields
	if req.ChatID == 0 {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "chat_id is required",
		})
		return
	}

	if req.Text == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "text is required",
		})
		return
	}

	err := h.service.SendMessage(c.Request.Context(), req.ChatID, req.Text)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error":   "Failed to send message",
			"details": err.Error(),
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "Message sent successfully",
	})
}

// CreateChat handles creating a new chat mapping
// POST /telegram/chat
func (h *Handler) CreateChat(c *gin.Context) {
	var req CreateChatRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error":   "Invalid request format",
			"details": err.Error(),
		})
		return
	}

	// Validate required fields
	if req.ChatID == 0 {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "chat_id is required",
		})
		return
	}

	if req.ChatUUID == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "chat_uuid is required",
		})
		return
	}

	// Validate UUID format
	if _, err := uuid.Parse(req.ChatUUID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "chat_uuid must be a valid UUID",
		})
		return
	}

	chatID, err := h.service.CreateChat(c.Request.Context(), req.ChatID, req.ChatUUID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error":   "Failed to create chat",
			"details": err.Error(),
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"chat_id": chatID,
		"message": "Chat created successfully",
	})
}

// GetChatURL handles generating a Telegram bot URL for a chat
// GET /telegram/chat-url?chat_uuid=<uuid>&bot_name=<name>
func (h *Handler) GetChatURL(c *gin.Context) {
	chatUUID := c.Query("chat_uuid")
	botName := c.DefaultQuery("bot_name", TelegramBotName)

	if chatUUID == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "chat_uuid query parameter is required",
		})
		return
	}

	// Validate UUID format
	if _, err := uuid.Parse(chatUUID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "chat_uuid must be a valid UUID",
		})
		return
	}

	url := GetChatURL(botName, chatUUID)

	c.JSON(http.StatusOK, ChatURLResponse{
		URL: url,
	})
}

// Subscribe handles starting a WebSocket subscription for a chat
// POST /telegram/subscribe
func (h *Handler) Subscribe(c *gin.Context) {
	var req SubscribeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error":   "Invalid request format",
			"details": err.Error(),
		})
		return
	}

	if req.ChatUUID == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "chat_uuid is required",
		})
		return
	}

	// Validate UUID format
	if _, err := uuid.Parse(req.ChatUUID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "chat_uuid must be a valid UUID",
		})
		return
	}

	// Start subscription in a goroutine so we can return immediately
	go func() {
		ctx := context.Background()
		err := h.service.Subscribe(ctx, req.ChatUUID)
		if err != nil {
			h.service.Logger.Error("Subscription failed", "error", err, "chat_uuid", req.ChatUUID)
		}
	}()

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "Subscription started for chat UUID: " + req.ChatUUID,
	})
}

// PostMessage handles posting a message via GraphQL mutation
// POST /telegram/post-message
func (h *Handler) PostMessage(c *gin.Context) {
	var req struct {
		ChatUUID string `json:"chat_uuid" binding:"required"`
		Text     string `json:"text" binding:"required"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error":   "Invalid request format",
			"details": err.Error(),
		})
		return
	}

	// Validate UUID format
	if _, err := uuid.Parse(req.ChatUUID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "chat_uuid must be a valid UUID",
		})
		return
	}

	response, err := h.service.PostMessage(c.Request.Context(), req.ChatUUID, req.Text)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error":   "Failed to post message",
			"details": err.Error(),
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"success":  true,
		"response": response,
	})
}

// Status handles health check for the Telegram service
// GET /telegram/status
func (h *Handler) Status(c *gin.Context) {
	status := gin.H{
		"service": "telegram",
		"status":  "running",
		"token":   h.service.Token != "", // Don't expose the actual token
	}

	if h.service.NatsClient != nil {
		status["nats"] = "connected"
	}

	c.JSON(http.StatusOK, status)
}

// GetMessages handles retrieving messages for a specific chat UUID
// GET /telegram/messages/{uuid}
func (h *Handler) GetMessages(c *gin.Context) {
	chatUUID := c.Param("uuid")
	since := c.Query("since")

	if chatUUID == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "chat UUID is required",
		})
		return
	}

	// Validate UUID format
	if _, err := uuid.Parse(chatUUID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "chat_uuid must be a valid UUID",
		})
		return
	}

	// For now, return empty messages array since we don't have message storage yet
	// TODO: Implement actual message retrieval from database/storage
	h.service.Logger.Info("Getting messages for chat UUID", "chat_uuid", chatUUID, "since", since)

	c.JSON(http.StatusOK, gin.H{
		"success":   true,
		"chat_uuid": chatUUID,
		"messages":  []interface{}{}, // Empty array for now
		"since":     since,
		"message":   "Message retrieval not yet implemented",
	})
}

// StartPolling handles starting the Telegram polling service
// POST /telegram/start-polling
func (h *Handler) StartPolling(c *gin.Context) {
	// This could be enhanced to manage the polling lifecycle
	go func() {
		ctx := context.Background()
		err := h.service.Start(ctx)
		if err != nil {
			h.service.Logger.Error("Polling failed", "error", err)
		}
	}()

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "Telegram polling started",
	})
}
