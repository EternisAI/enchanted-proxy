package invitecode

import (
	"net/http"
	"strconv"

	"github.com/eternisai/enchanted-proxy/pkg/auth"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

type Handler struct {
	service *Service
}

func NewHandler(service *Service) *Handler {
	return &Handler{
		service: service,
	}
}

// RedeemInviteCodeRequest represents the request body for redeeming an invite code with OAuth.
type RedeemInviteCodeRequest struct {
	AccessToken string `json:"access_token" binding:"required"`
}

// RedeemInviteCode handles redeeming an invite code with OAuth verification
// POST /api/v1/invites/:code/redeem.
func (h *Handler) RedeemInviteCode(c *gin.Context) {
	code := c.Param("code")

	var req RedeemInviteCodeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "accessToken & code required"})
		return
	}

	// Get user UUID from Firebase auth context
	userUUID, ok := auth.GetUserUUID(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "User not authenticated"})
		return
	}

	isWhitelisted, err := h.service.IsEmailWhitelisted(userUUID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	if isWhitelisted {
		c.JSON(http.StatusForbidden, gin.H{"error": "Email already whitelisted"})
		return
	}

	// Use invite code with the verified email
	if err := h.service.UseInviteCode(code, userUUID); err != nil {
		if err.Error() == "invite code not found" {
			c.JSON(http.StatusNotFound, gin.H{"error": "Invalid code"})
			return
		}
		if err.Error() == "invite code already used" {
			c.JSON(http.StatusConflict, gin.H{"error": "Code already used"})
			return
		}
		if err.Error() == "code bound to a different email" {
			c.JSON(http.StatusForbidden, gin.H{"error": "Code bound to a different email"})
			return
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// DeleteInviteCode handles deleting an invite code
// DELETE /api/v1/invites/:id.
func (h *Handler) DeleteInviteCode(c *gin.Context) {
	idStr := c.Param("id")
	id, err := strconv.ParseUint(idStr, 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid ID"})
		return
	}

	if err := h.service.DeleteInviteCode(uint(id)); err != nil {
		if err == gorm.ErrRecordNotFound {
			c.JSON(http.StatusNotFound, gin.H{"error": "Invalid code"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "Invite code deleted successfully"})
}

// ResetInviteCode handles resetting an invite code
// GET /api/v1/invites/reset/:code.
func (h *Handler) ResetInviteCode(c *gin.Context) {
	code := c.Param("code")

	if err := h.service.ResetInviteCode(code); err != nil {
		if err == gorm.ErrRecordNotFound {
			c.JSON(http.StatusNotFound, gin.H{"error": "Invalid code"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "Invite code reset successfully"})
}

// CheckEmailWhitelist checks if an email is whitelisted
// GET /api/v1/invites/:email/whitelist.
func (h *Handler) CheckEmailWhitelist(c *gin.Context) {
	email := c.Param("email")

	// Check if email is whitelisted (has valid invite codes)
	isWhitelisted, err := h.service.IsEmailWhitelisted(email)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"email":       email,
		"whitelisted": isWhitelisted,
	})
}
