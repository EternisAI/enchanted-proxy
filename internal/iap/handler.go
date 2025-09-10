package iap

import (
	"net/http"

	"github.com/eternisai/enchanted-proxy/internal/auth"
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
// Request body: { "signedTransactionInfo": "<JWS>" }
func (h *Handler) AttachAppStoreSubscription(c *gin.Context) {
	var body struct {
		SignedTransactionInfo string `json:"signedTransactionInfo"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || body.SignedTransactionInfo == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}

	userID, ok := auth.GetUserUUID(c)
	if !ok || userID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}

	payload, expiresAt, err := h.service.AttachAppStoreSubscription(c.Request.Context(), userID, body.SignedTransactionInfo)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid signedTransactionInfo"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"status":        true,
		"productId":     payload.ProductID,
		"originalTxId":  payload.OriginalTransactionId,
		"transactionId": payload.TransactionID,
		"expiresAt":     expiresAt,
	})
}
