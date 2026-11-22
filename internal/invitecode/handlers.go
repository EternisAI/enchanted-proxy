package invitecode

import (
	"net/http"

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

// CheckUserWhitelist checks whether the caller is whitelisted. It answers only
// for the caller's own user ID.
// GET /api/v1/invites/:userID/whitelist.
func (h *Handler) CheckUserWhitelist(c *gin.Context) {
	userID := c.Param("userID")

	callerID, ok := auth.GetUserID(c)
	if !ok {
		errors.Unauthorized(c, "User not authenticated", nil)
		return
	}
	if userID != callerID {
		c.AbortWithStatusJSON(http.StatusForbidden, errors.NewAPIError("You can only check your own whitelist status", nil))
		return
	}

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
