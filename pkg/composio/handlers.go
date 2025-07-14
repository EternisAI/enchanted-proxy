package composio

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

type Handler struct {
	service *Service
}

// NewComposioHandler creates a new ComposioHandler instance.
func NewHandler(service *Service) *Handler {
	return &Handler{
		service: service,
	}
}

// CreateConnectedAccount handles the creation of a new connected account
// POST /composio/connect.
func (h *Handler) CreateConnectedAccount(c *gin.Context) {
	var req CreateConnectedAccountRequest
	if err := c.ShouldBindJSON(&req); err != nil {
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

	// Call the service
	response, err := h.service.CreateConnectedAccount(req.UserID, req.Provider, req.RedirectURI)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error":   "Failed to create connected account",
			"details": err.Error(),
		})
		return
	}

	c.JSON(http.StatusOK, response)
}

func (h *Handler) GetConnectedAccount(c *gin.Context) {
	accountID := c.Query("account_id")
	if accountID == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "account_id is required",
		})
		return
	}

	response, err := h.service.GetConnectedAccount(accountID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error":   "Failed to get connected account",
			"details": err.Error(),
		})
		return
	}

	c.JSON(http.StatusOK, response)
}

func (h *Handler) RefreshToken(c *gin.Context) {
	accountID := c.Query("account_id")
	if accountID == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "account_id is required in query params",
		})
	}

	response, err := h.service.RefreshToken(accountID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error":   "Failed to refresh token",
			"details": err.Error(),
		})
		return
	}

	c.JSON(http.StatusOK, response)
}
