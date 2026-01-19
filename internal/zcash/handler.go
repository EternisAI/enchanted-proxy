package zcash

import (
	"errors"
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

// CreateInvoiceRequest is the request body for creating an invoice.
type CreateInvoiceRequest struct {
	ProductID string `json:"product_id" binding:"required"`
}

// InvoiceResponse is returned when creating or fetching an invoice.
type InvoiceResponse struct {
	InvoiceID string  `json:"invoice_id"`
	Address   string  `json:"address"`
	ProductID string  `json:"product_id"`
	PriceUSD  float64 `json:"price_usd"`
	ZecAmount float64 `json:"zec_amount"`
	ZatAmount int64   `json:"zat_amount"`
	Status    string  `json:"status"`
}

// POST /api/v1/zcash/invoice
func (h *Handler) CreateInvoice(c *gin.Context) {
	var req CreateInvoiceRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request: " + err.Error()})
		return
	}

	userID, ok := auth.GetUserID(c)
	if !ok || userID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}

	invoice, err := h.service.CreateInvoice(c.Request.Context(), userID, req.ProductID)
	if err != nil {
		h.logger.Error("failed to create zcash invoice", "error", err.Error(), "user_id", userID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create invoice"})
		return
	}

	c.JSON(http.StatusOK, InvoiceResponse{
		InvoiceID: invoice.ID.String(),
		Address:   invoice.ReceivingAddress,
		ProductID: invoice.ProductID,
		PriceUSD:  invoice.PriceUSD,
		ZecAmount: invoice.ZecAmount,
		ZatAmount: invoice.AmountZatoshis,
		Status:    invoice.Status,
	})
}

// GET /api/v1/zcash/invoice/:invoiceId
func (h *Handler) GetInvoice(c *gin.Context) {
	invoiceID := c.Param("invoiceId")
	if invoiceID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invoice_id required"})
		return
	}

	userID, ok := auth.GetUserID(c)
	if !ok || userID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}

	invoice, err := h.service.GetInvoiceForUser(c.Request.Context(), invoiceID, userID)
	if err != nil {
		h.logger.Error("failed to get invoice", "error", err.Error(), "invoice_id", invoiceID)
		if errors.Is(err, ErrInvalidInvoiceID) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid invoice ID"})
		} else if errors.Is(err, ErrInvoiceNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "invoice not found"})
		} else {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		}
		return
	}

	c.JSON(http.StatusOK, InvoiceResponse{
		InvoiceID: invoice.ID.String(),
		Address:   invoice.ReceivingAddress,
		ProductID: invoice.ProductID,
		PriceUSD:  invoice.PriceUSD,
		ZecAmount: invoice.ZecAmount,
		ZatAmount: invoice.AmountZatoshis,
		Status:    invoice.Status,
	})
}

// GET /api/v1/zcash/products
func (h *Handler) GetProducts(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"products": h.service.GetProducts()})
}

// CallbackRequest is sent by zcash-payment-backend when payment status changes.
type CallbackRequest struct {
	InvoiceID           string `json:"invoice_id" binding:"required"`
	Status              string `json:"status" binding:"required"` // "processing" | "paid"
	AccumulatedZatoshis int64  `json:"accumulated_zatoshis"`
}

// POST /internal/zcash/callback (called by zcash-payment-backend)
func (h *Handler) HandleCallback(c *gin.Context) {
	var req CallbackRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}

	err := h.service.HandlePaymentCallback(c.Request.Context(), req.InvoiceID, req.Status, req.AccumulatedZatoshis)
	if err != nil {
		h.logger.Error("failed to handle payment callback",
			"error", err.Error(),
			"invoice_id", req.InvoiceID,
			"status", req.Status,
		)
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}
