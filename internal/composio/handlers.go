package composio

import (
	"log/slog"
	"net/http"

	"github.com/eternisai/enchanted-proxy/internal/logger"
	"github.com/gin-gonic/gin"
)

type Handler struct {
	service *Service
	logger  *logger.Logger
}

// NewHandler creates a new ComposioHandler instance.
func NewHandler(service *Service, logger *logger.Logger) *Handler {
	return &Handler{
		service: service,
		logger:  logger,
	}
}

// CreateConnectedAccount handles the creation of a new connected account
// POST /composio/connect.
func (h *Handler) CreateConnectedAccount(c *gin.Context) {
	log := h.logger.WithContext(c.Request.Context()).WithComponent("composio_handler")

	var req CreateConnectedAccountRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		log.Error("invalid create connected account request", slog.String("error", err.Error()))
		c.JSON(http.StatusBadRequest, gin.H{
			"error":   "Invalid request format",
			"details": err.Error(),
		})
		return
	}

	// Validate required fields
	if req.UserID == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "user_id is required",
		})
		return
	}

	if req.Provider == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "provider is required",
		})
		return
	}

	log.Info("creating composio connected account",
		slog.String("user_id", req.UserID),
		slog.String("provider", req.Provider))

	// Call the service
	response, err := h.service.CreateConnectedAccount(req.UserID, req.Provider, req.RedirectURI)
	if err != nil {
		log.Error("failed to create connected account", slog.String("error", err.Error()))
		c.JSON(http.StatusInternalServerError, gin.H{
			"error":   "Failed to create connected account",
			"details": err.Error(),
		})
		return
	}

	log.Info("connected account created successfully", slog.String("account_id", response.ID))
	c.JSON(http.StatusOK, response)
}

func (h *Handler) GetConnectedAccount(c *gin.Context) {
	log := h.logger.WithContext(c.Request.Context()).WithComponent("composio_handler")

	accountID := c.Query("account_id")
	if accountID == "" {
		log.Warn("missing account_id in get connected account request")
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "account_id is required",
		})
		return
	}

	log.Info("getting composio connected account", slog.String("account_id", accountID))

	response, err := h.service.GetConnectedAccount(accountID)
	if err != nil {
		log.Error("failed to get connected account", slog.String("error", err.Error()))
		c.JSON(http.StatusInternalServerError, gin.H{
			"error":   "Failed to get connected account",
			"details": err.Error(),
		})
		return
	}

	log.Info("connected account retrieved successfully", slog.String("account_id", accountID))
	c.JSON(http.StatusOK, response)
}

func (h *Handler) RefreshToken(c *gin.Context) {
	log := h.logger.WithContext(c.Request.Context()).WithComponent("composio_handler")

	accountID := c.Query("account_id")
	if accountID == "" {
		log.Warn("missing account_id in refresh token request")
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "account_id is required in query params",
		})
		return
	}

	log.Info("refreshing composio token", slog.String("account_id", accountID))

	response, err := h.service.RefreshToken(accountID)
	if err != nil {
		log.Error("failed to refresh token", slog.String("error", err.Error()))
		c.JSON(http.StatusInternalServerError, gin.H{
			"error":   "Failed to refresh token",
			"details": err.Error(),
		})
		return
	}

	log.Info("token refreshed successfully", slog.String("account_id", accountID))
	c.JSON(http.StatusOK, response)
}
