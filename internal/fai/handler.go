package fai

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/eternisai/enchanted-proxy/internal/auth"
	apierrors "github.com/eternisai/enchanted-proxy/internal/errors"
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

// CreatePaymentIntentRequest is the request body for creating a FAI payment intent.
type CreatePaymentIntentRequest struct {
	ProductID string `json:"product_id" binding:"required"`
}

// PaymentIntentAPIResponse is returned to the client.
type PaymentIntentAPIResponse struct {
	PaymentID    string  `json:"payment_id"`
	ProductID    string  `json:"product_id"`
	PriceUSD     float64 `json:"price_usd"`
	FaiPrice     float64 `json:"fai_price"`
	FaiAmount    float64 `json:"fai_amount"`
	FaiAmountWei string  `json:"fai_amount_wei"`
	PaymentIDHex string  `json:"payment_id_hex"`
	Status       string  `json:"status"`
}

// GET /api/v1/fai/products
func (h *Handler) GetProducts(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"products": h.service.GetProducts()})
}

// POST /api/v1/fai/payment-intent
func (h *Handler) CreatePaymentIntent(c *gin.Context) {
	var req CreatePaymentIntentRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		apierrors.BadRequest(c, "invalid request: "+err.Error(), nil)
		return
	}

	userID, ok := auth.GetUserID(c)
	if !ok || userID == "" {
		apierrors.Unauthorized(c, "unauthorized", nil)
		return
	}

	intent, err := h.service.CreatePaymentIntent(c.Request.Context(), userID, req.ProductID)
	if err != nil {
		if errors.Is(err, ErrInvalidProduct) {
			apierrors.BadRequest(c, "invalid product_id", nil)
			return
		}
		h.logger.Error("failed to create FAI payment intent", slog.String("error", err.Error()), slog.String("user_id", userID))
		apierrors.Internal(c, "failed to create payment intent", nil)
		return
	}

	c.JSON(http.StatusOK, PaymentIntentAPIResponse{
		PaymentID:    intent.PaymentID,
		ProductID:    intent.ProductID,
		PriceUSD:     intent.PriceUSD,
		FaiPrice:     intent.FaiPrice,
		FaiAmount:    intent.FaiAmount,
		FaiAmountWei: intent.FaiAmountWei,
		PaymentIDHex: intent.PaymentIDHex,
		Status:       intent.Status,
	})
}

// GET /api/v1/fai/payment-intent/:paymentId
func (h *Handler) GetPaymentIntent(c *gin.Context) {
	paymentID := c.Param("paymentId")
	if paymentID == "" {
		apierrors.BadRequest(c, "payment_id required", nil)
		return
	}

	userID, ok := auth.GetUserID(c)
	if !ok || userID == "" {
		apierrors.Unauthorized(c, "unauthorized", nil)
		return
	}

	intent, err := h.service.GetPaymentStatus(c.Request.Context(), paymentID, userID)
	if err != nil {
		if errors.Is(err, ErrPaymentNotFound) {
			apierrors.NotFound(c, "payment intent not found", nil)
			return
		}
		h.logger.Error("failed to get payment intent", slog.String("error", err.Error()), slog.String("payment_id", paymentID))
		apierrors.Internal(c, "internal error", nil)
		return
	}

	c.JSON(http.StatusOK, PaymentIntentAPIResponse{
		PaymentID:    intent.PaymentID,
		ProductID:    intent.ProductID,
		PriceUSD:     intent.PriceUSD,
		FaiPrice:     intent.FaiPrice,
		FaiAmount:    intent.FaiAmount,
		FaiAmountWei: intent.FaiAmountWei,
		PaymentIDHex: intent.PaymentIDHex,
		Status:       intent.Status,
	})
}

// GET /api/v1/fai/config
func (h *Handler) GetConfig(c *gin.Context) {
	cfg, err := h.service.GetConfig()
	if err != nil {
		h.logger.Error("failed to get FAI config", slog.String("error", err.Error()))
		apierrors.Internal(c, "FAI payments not configured", nil)
		return
	}

	c.JSON(http.StatusOK, cfg)
}
