package oauth

import (
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"
)

type Handler struct {
	service *Service
}

func NewHandler(service *Service) *Handler {
	return &Handler{
		service: service,
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

	fmt.Println("ExchangeTokenReq: ", req)
	tokenResponse, err := h.service.ExchangeToken(req)
	if err != nil {
		c.JSON(http.StatusBadRequest, ErrorResponse{
			Error:       "exchange_failed",
			Description: err.Error(),
			Code:        http.StatusBadRequest,
		})
		return
	}

	c.JSON(http.StatusOK, tokenResponse)
}

// RefreshToken handles token refresh endpoint
// POST /auth/refresh.
func (h *Handler) RefreshToken(c *gin.Context) {
	var req RefreshTokenRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, ErrorResponse{
			Error:       "invalid_request",
			Description: err.Error(),
			Code:        http.StatusBadRequest,
		})
		return
	}

	tokenResponse, err := h.service.RefreshToken(req.Platform, req.RefreshToken)
	if err != nil {
		c.JSON(http.StatusBadRequest, ErrorResponse{
			Error:       "refresh_failed",
			Description: err.Error(),
			Code:        http.StatusBadRequest,
		})
		return
	}

	c.JSON(http.StatusOK, tokenResponse)
}
