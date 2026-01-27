package invitecode

import (
	"net/http"
	"strconv"

	"github.com/eternisai/enchanted-proxy/internal/auth"
	"github.com/eternisai/enchanted-proxy/internal/errors"
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
		errors.BadRequest(c, "accessToken & code required", nil)
		return
	}

	userID, ok := auth.GetUserID(c)
	if !ok {
		errors.Unauthorized(c, "User not authenticated", nil)
		return
	}

	isWhitelisted, err := h.service.IsUserWhitelisted(userID)
	if err != nil {
		errors.Internal(c, err.Error(), nil)
		return
	}

	if isWhitelisted {
		errors.AbortWithForbidden(c, errors.InviteAlreadyUsed())
		return
	}

	// Use invite code with the verified email
	if err := h.service.UseInviteCode(code, userID); err != nil {
		if err.Error() == "invite code not found" {
			errors.NotFound(c, "Invalid code", nil)
			return
		}
		if err.Error() == "invite code already used" {
			errors.Conflict(c, "Code already used", nil)
			return
		}
		if err.Error() == "code bound to a different user" {
			errors.AbortWithForbidden(c, errors.InviteWrongUser())
			return
		}
		errors.BadRequest(c, err.Error(), nil)
		return
	}

	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// DeleteInviteCode handles deleting an invite code
// DELETE /api/v1/invites/:id.
func (h *Handler) DeleteInviteCode(c *gin.Context) {
	idStr := c.Param("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		errors.BadRequest(c, "Invalid ID", nil)
		return
	}

	if err := h.service.DeleteInviteCode(id); err != nil {
		errors.Internal(c, err.Error(), nil)
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "Invite code deleted successfully"})
}

// ResetInviteCode handles resetting an invite code
// GET /api/v1/invites/reset/:code.
func (h *Handler) ResetInviteCode(c *gin.Context) {
	code := c.Param("code")

	if err := h.service.ResetInviteCode(code); err != nil {
		errors.Internal(c, err.Error(), nil)
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "Invite code reset successfully"})
}

// CheckUserWhitelist checks if a user ID is whitelisted
// GET /api/v1/invites/:userID/whitelist.
func (h *Handler) CheckUserWhitelist(c *gin.Context) {
	userID := c.Param("userID")

	// Check if user ID is whitelisted (has valid invite codes)
	isWhitelisted, err := h.service.IsUserWhitelisted(userID)
	if err != nil {
		errors.Internal(c, err.Error(), nil)
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"userID":      userID,
		"whitelisted": isWhitelisted,
	})
}
