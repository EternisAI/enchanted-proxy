package stripe

import (
	stderrors "errors"
	"io"
	"log/slog"
	"net/http"

	"github.com/eternisai/enchanted-proxy/internal/auth"
	"github.com/eternisai/enchanted-proxy/internal/errors"
	"github.com/eternisai/enchanted-proxy/internal/logger"
	"github.com/gin-gonic/gin"
)

// Handler provides HTTP endpoints for Stripe integration.
// It handles two main operations:
// 1. Creating Checkout Sessions for web app users (authenticated)
// 2. Processing webhook events from Stripe (public, signature-verified)
type Handler struct {
	logger  *logger.Logger
	service *Service
}

// NewHandler creates a new Stripe HTTP handler instance.
//
// Parameters:
//   - service: Stripe service for business logic
//   - logger: Structured logger for HTTP operations
//
// Returns:
//   - *Handler: Initialized Stripe handler
func NewHandler(service *Service, logger *logger.Logger) *Handler {
	return &Handler{
		logger:  logger.WithComponent("stripe_handler"),
		service: service,
	}
}

// CreateCheckoutSession generates a Stripe Checkout Session URL for Pro subscription.
//
// Endpoint: POST /stripe/create-checkout-session
// Authentication: Required (Firebase token)
// Content-Type: application/json
//
// Request Body:
//
//	{
//	  "priceId": "price_1SZsJOBX38twNhdvGLmXmvm7"
//	}
//
// Response (200 OK):
//
//	{
//	  "url": "https://checkout.stripe.com/c/pay/cs_test_..."
//	}
//
// Response (400 Bad Request):
//
//	{
//	  "error": "invalid request"
//	}
//
// Response (401 Unauthorized):
//
//	{
//	  "error": "unauthorized"
//	}
//
// Response (500 Internal Server Error):
//
//	{
//	  "error": "failed to create checkout session"
//	}
//
// Flow:
// 1. Validate request body and extract priceId
// 2. Extract authenticated user ID from JWT token
// 3. Call Stripe API to create Checkout Session
// 4. Return Checkout Session URL for client-side redirect
//
// Security:
//   - Requires valid Firebase authentication token
//   - User ID is extracted from verified JWT (cannot be spoofed)
//   - Price ID validation is handled by Stripe API
func (h *Handler) CreateCheckoutSession(c *gin.Context) {
	log := h.logger.WithContext(c.Request.Context())

	var body struct {
		PriceID string `json:"priceId" binding:"required"`
	}

	if err := c.ShouldBindJSON(&body); err != nil {
		log.Error("invalid request body", slog.String("error", err.Error()))
		errors.BadRequest(c, "invalid request", nil)
		return
	}

	userID, ok := auth.GetUserID(c)
	if !ok || userID == "" {
		log.Error("unauthorized request - missing user ID")
		errors.Unauthorized(c, "unauthorized", nil)
		return
	}

	// Determine redirect URLs from request origin
	origin := c.GetHeader("Origin")
	if origin == "" {
		origin = c.GetHeader("Referer")
	}
	// Default to production domain if no origin/referer
	if origin == "" {
		origin = "https://silo.freysa.ai"
	}

	sessionURL, err := h.service.CreateCheckoutSession(c.Request.Context(), userID, body.PriceID, origin)
	if err != nil {
		log.Error("failed to create checkout session",
			slog.String("user_id", userID),
			slog.String("price_id", body.PriceID),
			slog.String("error", err.Error()))
		errors.Internal(c, "failed to create checkout session", nil)
		return
	}

	log.Info("checkout session created",
		slog.String("user_id", userID),
		slog.String("price_id", body.PriceID),
		slog.String("session_url", sessionURL))

	c.JSON(http.StatusOK, gin.H{"url": sessionURL})
}

// CreatePortalSession generates a Stripe Billing Portal Session URL for subscription management.
//
// Endpoint: POST /api/v1/stripe/create-portal-session
// Authentication: Required (Firebase token)
// Content-Type: application/json
//
// Request Body: (empty - no body required)
//
// Response (200 OK):
//
//	{
//	  "url": "https://billing.stripe.com/session/bps_..."
//	}
//
// Response (400 Bad Request):
//
//	{
//	  "error": "no stripe customer found"
//	}
//
// Response (401 Unauthorized):
//
//	{
//	  "error": "unauthorized"
//	}
//
// Response (500 Internal Server Error):
//
//	{
//	  "error": "failed to create portal session"
//	}
//
// Flow:
// 1. Extract authenticated user ID from JWT token
// 2. Retrieve Stripe customer ID from database
// 3. Determine return URL from request origin (e.g., https://silo.freysa.ai/settings)
// 4. Call Stripe API to create Billing Portal Session
// 5. Return portal URL for client-side redirect
//
// Security:
//   - Requires valid Firebase authentication token
//   - User ID is extracted from verified JWT (cannot be spoofed)
//   - Customer ID is retrieved from database (trusted source)
//
// Example Usage:
//
//	// From web app
//	fetch('/api/v1/stripe/create-portal-session', {
//	  method: 'POST',
//	  headers: { 'Authorization': 'Bearer <token>' }
//	})
//	.then(res => res.json())
//	.then(data => window.location.href = data.url)
func (h *Handler) CreatePortalSession(c *gin.Context) {
	log := h.logger.WithContext(c.Request.Context())

	// Extract user ID from auth token
	userID, ok := auth.GetUserID(c)
	if !ok || userID == "" {
		log.Error("unauthorized request - missing user ID")
		errors.Unauthorized(c, "unauthorized", nil)
		return
	}

	// Determine return URL from request origin
	origin := c.GetHeader("Origin")
	if origin == "" {
		origin = c.GetHeader("Referer")
	}
	// Default to production domain if no origin/referer
	if origin == "" {
		origin = "https://silo.freysa.ai"
	}
	returnURL := origin + "/settings"

	// Create portal session
	portalURL, err := h.service.CreatePortalSession(c.Request.Context(), userID, returnURL)
	if err != nil {
		log.Error("failed to create portal session",
			slog.String("user_id", userID),
			slog.String("error", err.Error()))

		// Check if error is due to missing customer ID
		if stderrors.Is(err, ErrNoCustomerID) {
			errors.BadRequest(c, "no stripe customer found", nil)
			return
		}

		errors.Internal(c, "failed to create portal session", nil)
		return
	}

	log.Info("portal session created",
		slog.String("user_id", userID),
		slog.String("portal_url", portalURL),
		slog.String("return_url", returnURL))

	c.JSON(http.StatusOK, gin.H{"url": portalURL})
}

// HandleWebhook processes incoming Stripe webhook events.
//
// Endpoint: POST /stripe/webhook
// Authentication: None (signature verification in handler)
// Content-Type: application/json
// Headers:
//   - Stripe-Signature: Required (webhook signature)
//
// Request Body: Raw Stripe webhook event JSON (various event types)
//
// Response (200 OK):
//
//	{
//	  "status": "success"
//	}
//
// Response (400 Bad Request):
//
//	{
//	  "error": "invalid payload" | "missing signature"
//	}
//
// Response (200 OK with error):
//
//	{
//	  "error": "signature verification failed: ..."
//	}
//
// Flow:
// 1. Read raw request body (must be unmodified for signature verification)
// 2. Extract Stripe-Signature header
// 3. Verify webhook signature using Stripe SDK
// 4. Route event to appropriate handler in service layer
// 5. Always return 200 OK to prevent Stripe retries (even on errors)
//
// Security:
//   - NO Firebase authentication required (Stripe cannot provide tokens)
//   - Security via cryptographic signature verification
//   - Signature is verified using STRIPE_WEBHOOK_SECRET
//   - Only events signed by Stripe will be processed
//
// Error Handling:
// - Always returns 200 OK to acknowledge receipt (prevents Stripe retries)
// - Signature verification failures are logged but return 200
// - Processing errors are logged but return 200
// - This prevents legitimate webhook failures from causing infinite retries
//
// Webhook Events Handled:
//   - checkout.session.completed: Grant Pro access
//   - customer.subscription.deleted: Revoke Pro access
//   - customer.subscription.updated: Update Pro expiration
//
// Testing:
// Use Stripe CLI for local testing:
//
//	stripe listen --forward-to http://localhost:8080/stripe/webhook
//	stripe trigger checkout.session.completed
func (h *Handler) HandleWebhook(c *gin.Context) {
	log := h.logger.WithContext(c.Request.Context())

	payload, err := io.ReadAll(c.Request.Body)
	if err != nil {
		log.Error("failed to read webhook payload", slog.String("error", err.Error()))
		errors.BadRequest(c, "invalid payload", nil)
		return
	}

	signature := c.GetHeader("Stripe-Signature")
	if signature == "" {
		log.Error("missing Stripe-Signature header")
		errors.BadRequest(c, "missing signature", nil)
		return
	}

	if err := h.service.HandleWebhook(c.Request.Context(), payload, signature); err != nil {
		log.Error("webhook processing failed", slog.String("error", err.Error()))
		// Always return 200 to Stripe to prevent retries for invalid signatures
		c.JSON(http.StatusOK, gin.H{"error": err.Error()})
		return
	}

	log.Info("webhook processed successfully")
	c.JSON(http.StatusOK, gin.H{"status": "success"})
}
