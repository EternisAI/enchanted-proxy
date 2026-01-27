package composio

import (
	"log/slog"
	"net/http"

	"github.com/eternisai/enchanted-proxy/internal/errors"
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
		errors.BadRequest(c, "Invalid request format", map[string]interface{}{"details": err.Error()})
		return
	}

	// Validate required fields
	if req.UserID == "" {
		errors.BadRequest(c, "user_id is required", nil)
		return
	}

	if req.Provider == "" {
		errors.BadRequest(c, "provider is required", nil)
		return
	}

	log.Info("creating composio connected account",
		slog.String("user_id", req.UserID),
		slog.String("provider", req.Provider))

	// Call the service
	response, err := h.service.CreateConnectedAccount(req.UserID, req.Provider, req.RedirectURI)
	if err != nil {
		log.Error("failed to create connected account", slog.String("error", err.Error()))
		errors.Internal(c, "Failed to create connected account", map[string]interface{}{"details": err.Error()})
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
		errors.BadRequest(c, "account_id is required", nil)
		return
	}

	log.Info("getting composio connected account", slog.String("account_id", accountID))

	response, err := h.service.GetConnectedAccount(accountID)
	if err != nil {
		log.Error("failed to get connected account", slog.String("error", err.Error()))
		errors.Internal(c, "Failed to get connected account", map[string]interface{}{"details": err.Error()})
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
		errors.BadRequest(c, "account_id is required in query params", nil)
		return
	}

	log.Info("refreshing composio token", slog.String("account_id", accountID))

	response, err := h.service.RefreshToken(accountID)
	if err != nil {
		log.Error("failed to refresh token", slog.String("error", err.Error()))
		errors.Internal(c, "Failed to refresh token", map[string]interface{}{"details": err.Error()})
		return
	}

	log.Info("token refreshed successfully", slog.String("account_id", accountID))
	c.JSON(http.StatusOK, response)
}
