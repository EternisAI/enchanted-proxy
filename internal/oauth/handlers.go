package oauth

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

func NewHandler(service *Service, logger *logger.Logger) *Handler {
	return &Handler{
		service: service,
		logger:  logger,
	}
}

// ExchangeToken handles token exchange endpoint
// POST /auth/exchange.
func (h *Handler) ExchangeToken(c *gin.Context) {
	var req TokenExchangeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, ErrorResponse{
			Error:       "invalid_request",
			Description: err.Error(),
			Code:        http.StatusBadRequest,
		})
		return
	}

	log := h.logger.WithContext(c.Request.Context()).WithComponent("oauth_handler")
	log.Info("oauth token exchange requested",
		slog.String("platform", req.Platform),
		slog.String("grant_type", req.GrantType))

	tokenResponse, err := h.service.ExchangeToken(req)
	if err != nil {
		log.Error("oauth token exchange failed", slog.String("error", err.Error()))
		c.JSON(http.StatusBadRequest, ErrorResponse{
			Error:       "exchange_failed",
			Description: err.Error(),
			Code:        http.StatusBadRequest,
		})
		return
	}

	log.Info("oauth token exchange successful", slog.String("platform", req.Platform))
	c.JSON(http.StatusOK, tokenResponse)
}

// RefreshToken handles token refresh endpoint
// POST /auth/refresh.
func (h *Handler) RefreshToken(c *gin.Context) {
	log := h.logger.WithContext(c.Request.Context()).WithComponent("oauth_handler")

	var req RefreshTokenRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		log.Error("invalid refresh token request", slog.String("error", err.Error()))
		c.JSON(http.StatusBadRequest, ErrorResponse{
			Error:       "invalid_request",
			Description: err.Error(),
			Code:        http.StatusBadRequest,
		})
		return
	}

	log.Info("oauth token refresh requested", slog.String("platform", req.Platform))
	tokenResponse, err := h.service.RefreshToken(req.Platform, req.RefreshToken)
	if err != nil {
		log.Error("oauth token refresh failed", slog.String("error", err.Error()))
		c.JSON(http.StatusBadRequest, ErrorResponse{
			Error:       "refresh_failed",
			Description: err.Error(),
			Code:        http.StatusBadRequest,
		})
		return
	}

	log.Info("oauth token refresh successful", slog.String("platform", req.Platform))
	c.JSON(http.StatusOK, tokenResponse)
}
