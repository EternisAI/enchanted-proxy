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

// GetToolBySlug handles retrieving toolkit information by slug
// GET /composio/tools/:slug.
func (h *Handler) GetToolBySlug(c *gin.Context) {
	slug := c.Param("slug")
	if slug == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "toolkit slug is required",
		})
		return
	}

	// Call the service
	toolkit, err := h.service.GetToolBySlug(slug)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error":   "Failed to retrieve toolkit",
			"details": err.Error(),
		})
		return
	}

	c.JSON(http.StatusOK, toolkit)
}

// ExecuteTool handles tool execution
// POST /composio/tools/:slug/execute.
func (h *Handler) ExecuteTool(c *gin.Context) {
	slug := c.Param("slug")
	if slug == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "tool slug is required",
		})
		return
	}

	var req ExecuteToolRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error":   "Invalid request format",
			"details": err.Error(),
		})
		return
	}

	// Validate that either UserID or EntityID is provided
	if req.UserID == "" && req.EntityID == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "either user_id or entity_id is required",
		})
		return
	}

	// Call the service
	response, err := h.service.ExecuteTool(slug, req)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error":   "Failed to execute tool",
			"details": err.Error(),
		})
		return
	}

	c.JSON(http.StatusOK, response)
}

// GetConnectedAccounts handles retrieving connected accounts for a user
// GET /composio/accounts/:user_id.
func (h *Handler) GetConnectedAccounts(c *gin.Context) {
	userID := c.Param("user_id")
	if userID == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "user_id is required",
		})
		return
	}

	// Call the service
	accounts, err := h.service.GetConnectedAccountByUserID(userID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error":   "Failed to retrieve connected accounts",
			"details": err.Error(),
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"accounts": accounts,
		"count":    len(accounts),
	})
}

// HealthCheck provides a health check endpoint for the Composio service
// GET /composio/health.
func (h *Handler) HealthCheck(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"status":  "healthy",
		"service": "composio",
		"message": "Composio service is running",
	})
}
