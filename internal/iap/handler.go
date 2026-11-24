package iap

import (
	stderrors "errors"
	"log/slog"
	"net/http"

	"github.com/eternisai/enchanted-proxy/internal/auth"
	"github.com/eternisai/enchanted-proxy/internal/errors"
	"github.com/eternisai/enchanted-proxy/internal/logger"
	"github.com/gin-gonic/gin"
)

type Handler struct {
	logger  *logger.Logger
	service *Service
}

func NewHandler(service *Service, logger *logger.Logger) *Handler {
	return &Handler{logger: logger, service: service}
}

// AttachAppStoreSubscription validates a signed transaction JWS and marks user as Pro.
// Request body: { "jwsTransactionInfo": "<JWS>" }.
func (h *Handler) AttachAppStoreSubscription(c *gin.Context) {
	var body struct {
		JWSTransactionInfo string `json:"jwsTransactionInfo"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || body.JWSTransactionInfo == "" {
		errors.BadRequest(c, "invalid request", nil)
		return
	}

	userID, ok := auth.GetUserID(c)
	if !ok || userID == "" {
		errors.Unauthorized(c, "unauthorized", nil)
		return
	}

	var identity *auth.Identity
	if id, ok := auth.GetIdentity(c); ok {
		identity = &id
	}

	attached, err := h.service.AttachAppStoreSubscription(c.Request.Context(), userID, identity, body.JWSTransactionInfo)
	var refusal *Refusal
	switch {
	case stderrors.As(err, &refusal):
		h.logger.WithContext(c.Request.Context()).Info("purchase refused", slog.String("reason", refusal.Reason))
		errors.AbortWithForbidden(c, errors.NewForbiddenError(errors.ReasonPurchaseRefused, refusal.Message, refusal.Message, "", nil))
		return
	case stderrors.Is(err, ErrAppleUnavailable):
		c.JSON(http.StatusServiceUnavailable, errors.NewAPIError("The App Store could not be reached to confirm this purchase. Please try again later.", nil))
		return
	case err != nil:
		h.logger.WithContext(c.Request.Context()).Warn("purchase not attached", slog.String("error", err.Error()))
		errors.BadRequest(c, "invalid jwsTransactionInfo", nil)
		return
	}

	payload := attached.Transaction
	if attached.Blocked {
		// Same shape as an accepted purchase, as the app expects, but it grants
		// nothing.
		c.JSON(http.StatusOK, gin.H{
			"status":        false,
			"productId":     payload.ProductID,
			"originalTxId":  payload.OriginalTransactionId,
			"transactionId": payload.TransactionID,
		})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"status":        true,
		"productId":     payload.ProductID,
		"originalTxId":  payload.OriginalTransactionId,
		"transactionId": payload.TransactionID,
		"expiresAt":     attached.ExpiresAt,
	})
}
