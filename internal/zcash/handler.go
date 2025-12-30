package zcash

import (
	"net/http"
	"strings"

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

type CreateInvoiceRequestBody struct {
	ProductID string `json:"product_id" binding:"required"`
}

type CreateInvoiceResponseBody struct {
	InvoiceID string  `json:"invoice_id"`
	Address   string  `json:"address"`
	ProductID string  `json:"product_id"`
	PriceUSD  float64 `json:"price_usd"`
	ZecAmount float64 `json:"zec_amount"`
	ZatAmount int64   `json:"zat_amount"`
}

func (h *Handler) CreateInvoice(c *gin.Context) {
	var body CreateInvoiceRequestBody
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request: " + err.Error()})
		return
	}

	userID, ok := auth.GetUserID(c)
	if !ok || userID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}

	product := h.service.GetProduct(body.ProductID)
	if product == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "unknown product"})
		return
	}

	invoice, zecPriceUSD, err := h.service.CreateInvoice(c.Request.Context(), userID, body.ProductID)
	if err != nil {
		h.logger.Error("failed to create zcash invoice", "error", err.Error(), "user_id", userID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create invoice"})
		return
	}

	zecAmount := product.PriceUSD / zecPriceUSD
	zatAmount := int64(zecAmount * 100_000_000)

	c.JSON(http.StatusOK, CreateInvoiceResponseBody{
		InvoiceID: invoice.InvoiceID,
		Address:   invoice.Address,
		ProductID: body.ProductID,
		PriceUSD:  product.PriceUSD,
		ZecAmount: zecAmount,
		ZatAmount: zatAmount,
	})
}

func (h *Handler) GetInvoiceStatus(c *gin.Context) {
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

	parts := strings.Split(invoiceID, "_")
	if len(parts) == 0 || parts[0] != userID {
		c.JSON(http.StatusForbidden, gin.H{"error": "forbidden"})
		return
	}

	status, err := h.service.GetInvoiceStatus(c.Request.Context(), invoiceID)
	if err != nil {
		h.logger.Error("failed to get invoice status", "error", err.Error(), "invoice_id", invoiceID)
		c.JSON(http.StatusNotFound, gin.H{"error": "invoice not found"})
		return
	}

	c.JSON(http.StatusOK, status)
}

type ConfirmPaymentRequestBody struct {
	InvoiceID string `json:"invoice_id" binding:"required"`
}

func (h *Handler) ConfirmPayment(c *gin.Context) {
	var body ConfirmPaymentRequestBody
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}

	userID, ok := auth.GetUserID(c)
	if !ok || userID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}

	err := h.service.ConfirmPayment(c.Request.Context(), userID, body.InvoiceID)
	if err != nil {
		h.logger.Error("failed to confirm payment", "error", err.Error(), "user_id", userID, "invoice_id", body.InvoiceID)
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"status": "confirmed"})
}

func (h *Handler) GetProducts(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"products": h.service.GetProducts()})
}
