package stripe

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/eternisai/enchanted-proxy/internal/config"
	"github.com/eternisai/enchanted-proxy/internal/logger"
	pgdb "github.com/eternisai/enchanted-proxy/internal/storage/pg/sqlc"
	"github.com/stripe/stripe-go/v84"
	"github.com/stripe/stripe-go/v84/checkout/session"
	"github.com/stripe/stripe-go/v84/subscription"
	"github.com/stripe/stripe-go/v84/webhook"
)

// Service handles Stripe subscription management and webhook processing.
// It manages the lifecycle of Pro subscriptions for web app users, including:
// - Creating Stripe Checkout Sessions for new subscriptions
// - Processing webhook events for subscription state changes
// - Updating entitlements in the database based on subscription status
type Service struct {
	queries pgdb.Querier
	logger  *logger.Logger
}

// NewService creates a new Stripe service instance and configures the Stripe SDK.
// It sets the global Stripe API key from the application configuration.
//
// Parameters:
//   - queries: Database querier for managing entitlements
//   - logger: Structured logger for service operations
//
// Returns:
//   - *Service: Initialized Stripe service
func NewService(queries pgdb.Querier, logger *logger.Logger) *Service {
	log := logger.WithComponent("stripe_service")

	// Validate and set Stripe API key
	apiKey := config.AppConfig.StripeSecretKey
	if apiKey == "" {
		log.Warn("Stripe secret key is empty - API calls will fail")
	} else if len(apiKey) < 20 {
		log.Warn("Stripe secret key appears invalid (too short)", "length", len(apiKey))
	} else {
		// Log key prefix for debugging (first 7 chars: "sk_test" or "sk_live")
		prefix := apiKey
		if len(apiKey) > 12 {
			prefix = apiKey[:12] + "..." // Show "sk_test_xxxx..." or "sk_live_xxxx..."
		}
		log.Info("Stripe API key configured", "key_prefix", prefix, "key_length", len(apiKey))
	}

	stripe.Key = apiKey
	return &Service{
		queries: queries,
		logger:  log,
	}
}

// CreateCheckoutSession generates a Stripe Checkout Session URL for Pro subscription purchase.
// The session includes:
// - 3-day free trial with payment method required upfront
// - Automatic subscription creation after trial
// - Firebase user ID stored in subscription metadata for webhook processing
// - Dynamic success/cancel URLs based on the request origin
//
// Parameters:
//   - ctx: Context for the operation
//   - userID: Firebase user ID of the purchaser
//   - priceID: Stripe Price ID (e.g., price_1SZsJOBX38twNhdvGLmXmvm7)
//   - origin: Origin URL from the request (e.g., "https://silo.freysa.ai")
//
// Returns:
//   - string: Checkout Session URL for redirecting the user
//   - error: Any error encountered during session creation
//
// Example:
//
//	url, err := service.CreateCheckoutSession(ctx, "firebase_uid_123", "price_xxx", "https://silo.freysa.ai")
//	if err != nil {
//	    return err
//	}
//	// Redirect user to url in browser
func (s *Service) CreateCheckoutSession(ctx context.Context, userID string, priceID string, origin string) (string, error) {
	// Build success and cancel URLs dynamically from origin
	successURL := origin + "/?session_id={CHECKOUT_SESSION_ID}"
	cancelURL := origin + "/pricing?canceled=true"

	params := &stripe.CheckoutSessionParams{
		Mode: stripe.String(string(stripe.CheckoutSessionModeSubscription)),
		LineItems: []*stripe.CheckoutSessionLineItemParams{
			{
				Price:    stripe.String(priceID),
				Quantity: stripe.Int64(1),
			},
		},
		SuccessURL: stripe.String(successURL),
		CancelURL:  stripe.String(cancelURL),
		SubscriptionData: &stripe.CheckoutSessionSubscriptionDataParams{
			TrialPeriodDays: stripe.Int64(3), // 3-day free trial
			TrialSettings: &stripe.CheckoutSessionSubscriptionDataTrialSettingsParams{
				EndBehavior: &stripe.CheckoutSessionSubscriptionDataTrialSettingsEndBehaviorParams{
					MissingPaymentMethod: stripe.String("cancel"),
				},
			},
			Metadata: map[string]string{
				"firebase_user_id": userID,
			},
		},
	}

	sess, err := session.New(params)
	if err != nil {
		return "", fmt.Errorf("failed to create checkout session: %w", err)
	}

	s.logger.Info("checkout session created",
		"user_id", userID,
		"price_id", priceID,
		"session_id", sess.ID,
		"origin", origin,
		"success_url", successURL,
		"cancel_url", cancelURL)

	return sess.URL, nil
}

// HandleWebhook processes incoming Stripe webhook events with signature verification.
// This method:
// 1. Verifies the webhook signature to ensure authenticity
// 2. Routes the event to the appropriate handler based on event type
// 3. Updates entitlements in the database accordingly
//
// Supported webhook events:
//   - checkout.session.completed: Grants Pro access after successful payment
//   - customer.subscription.deleted: Revokes Pro access when subscription ends
//   - customer.subscription.updated: Updates Pro expiration on renewal or status change
//
// Parameters:
//   - ctx: Context for the operation
//   - payload: Raw webhook request body (must be unmodified for signature verification)
//   - signature: Value of the Stripe-Signature header
//
// Returns:
//   - error: Signature verification failures or processing errors
//
// Security:
// The webhook endpoint MUST NOT require authentication, as Stripe cannot provide tokens.
// Security is ensured through cryptographic signature verification using the webhook secret.
func (s *Service) HandleWebhook(ctx context.Context, payload []byte, signature string) error {
	event, err := webhook.ConstructEvent(payload, signature, config.AppConfig.StripeWebhookSecret)
	if err != nil {
		return fmt.Errorf("webhook signature verification failed: %w", err)
	}

	s.logger.Info("webhook event received", "type", event.Type, "event_id", event.ID)

	switch event.Type {
	case "checkout.session.completed":
		return s.handleCheckoutCompleted(ctx, event)
	case "customer.subscription.deleted":
		return s.handleSubscriptionDeleted(ctx, event)
	case "customer.subscription.updated":
		return s.handleSubscriptionUpdated(ctx, event)
	default:
		s.logger.Info("unhandled webhook event type", "type", event.Type)
	}

	return nil
}

// handleCheckoutCompleted grants Pro access when a checkout session is completed.
// This event fires when:
// - User completes payment and trial begins (for subscriptions with trial)
// - User completes payment and subscription becomes active (for no-trial subscriptions)
//
// The method:
// 1. Parses the checkout session from the webhook event
// 2. Retrieves the subscription details to get metadata and expiration
// 3. Extracts the firebase_user_id from subscription metadata
// 4. Updates the entitlements table with Pro expiration and provider
//
// Database updates:
//   - pro_expires_at: Set to subscription.current_period_end
//   - subscription_provider: Set to "stripe"
//
// Parameters:
//   - ctx: Context for the operation
//   - event: Stripe webhook event containing checkout session data
//
// Returns:
//   - error: Any error during session parsing, subscription retrieval, or database update
func (s *Service) handleCheckoutCompleted(ctx context.Context, event stripe.Event) error {
	var session stripe.CheckoutSession
	if err := json.Unmarshal(event.Data.Raw, &session); err != nil {
		return fmt.Errorf("failed to parse checkout session: %w", err)
	}

	// Retrieve subscription to get metadata and expiration
	sub, err := subscription.Get(session.Subscription.ID, nil)
	if err != nil {
		return fmt.Errorf("failed to retrieve subscription: %w", err)
	}

	userID, ok := sub.Metadata["firebase_user_id"]
	if !ok || userID == "" {
		return fmt.Errorf("missing firebase_user_id in subscription metadata")
	}

	// Get current period end from the first subscription item
	// Note: Subscriptions can have multiple items, but for our use case (single price),
	// the first item's period end represents the subscription's billing period end
	if sub.Items == nil || len(sub.Items.Data) == 0 {
		return fmt.Errorf("subscription has no items")
	}
	expiresAt := time.Unix(sub.Items.Data[0].CurrentPeriodEnd, 0)

	if err := s.queries.UpsertEntitlement(ctx, pgdb.UpsertEntitlementParams{
		UserID:               userID,
		ProExpiresAt:         sql.NullTime{Time: expiresAt, Valid: true},
		SubscriptionProvider: sql.NullString{String: "stripe", Valid: true},
	}); err != nil {
		return fmt.Errorf("failed to upsert entitlement: %w", err)
	}

	s.logger.Info("pro access granted",
		"user_id", userID,
		"subscription_id", sub.ID,
		"expires_at", expiresAt,
		"provider", "stripe")

	return nil
}

// handleSubscriptionDeleted revokes Pro access when a subscription is deleted.
// This event fires when:
// - User cancels subscription and billing period ends
// - Subscription is canceled due to payment failure
// - Admin manually cancels subscription in Stripe Dashboard
//
// The method:
// 1. Parses the subscription from the webhook event
// 2. Extracts the firebase_user_id from subscription metadata
// 3. Sets pro_expires_at to NULL in the entitlements table
//
// Database updates:
//   - pro_expires_at: Set to NULL (revokes access)
//   - subscription_provider: Set to "stripe" (maintains provider history)
//
// Parameters:
//   - ctx: Context for the operation
//   - event: Stripe webhook event containing subscription data
//
// Returns:
//   - error: Any error during subscription parsing or database update
func (s *Service) handleSubscriptionDeleted(ctx context.Context, event stripe.Event) error {
	var sub stripe.Subscription
	if err := json.Unmarshal(event.Data.Raw, &sub); err != nil {
		return fmt.Errorf("failed to parse subscription: %w", err)
	}

	userID, ok := sub.Metadata["firebase_user_id"]
	if !ok || userID == "" {
		return fmt.Errorf("missing firebase_user_id in subscription metadata")
	}

	// Set pro_expires_at to NULL to revoke access
	if err := s.queries.UpsertEntitlement(ctx, pgdb.UpsertEntitlementParams{
		UserID:               userID,
		ProExpiresAt:         sql.NullTime{Valid: false},
		SubscriptionProvider: sql.NullString{String: "stripe", Valid: true},
	}); err != nil {
		return fmt.Errorf("failed to revoke entitlement: %w", err)
	}

	s.logger.Info("pro access revoked",
		"user_id", userID,
		"subscription_id", sub.ID,
		"provider", "stripe")

	return nil
}

// handleSubscriptionUpdated updates Pro expiration when subscription status changes.
// This event fires when:
// - Subscription renews (monthly billing cycle)
// - Subscription status changes (active, past_due, canceled, etc.)
// - Subscription is modified (plan change, trial extension)
//
// The method:
// 1. Parses the subscription from the webhook event
// 2. Checks subscription status to determine access eligibility
// 3. Updates pro_expires_at based on status:
//   - active/trialing: Set to current_period_end
//   - past_due/canceled/unpaid: Set to NULL (revoke access)
//
// Database updates:
//   - pro_expires_at: Updated based on subscription status
//   - subscription_provider: Set to "stripe"
//
// Subscription status handling:
//   - active: Full access until current_period_end
//   - trialing: Full access until trial ends
//   - past_due: Access revoked (awaiting payment)
//   - canceled: Access revoked (user canceled)
//   - unpaid: Access revoked (payment failed)
//
// Parameters:
//   - ctx: Context for the operation
//   - event: Stripe webhook event containing updated subscription data
//
// Returns:
//   - error: Any error during subscription parsing or database update
func (s *Service) handleSubscriptionUpdated(ctx context.Context, event stripe.Event) error {
	var sub stripe.Subscription
	if err := json.Unmarshal(event.Data.Raw, &sub); err != nil {
		return fmt.Errorf("failed to parse subscription: %w", err)
	}

	userID, ok := sub.Metadata["firebase_user_id"]
	if !ok || userID == "" {
		return fmt.Errorf("missing firebase_user_id in subscription metadata")
	}

	var proExpiresAt sql.NullTime

	// Only set expiration if subscription is active or trialing
	if sub.Status == stripe.SubscriptionStatusActive || sub.Status == stripe.SubscriptionStatusTrialing {
		// Get current period end from the first subscription item
		if sub.Items != nil && len(sub.Items.Data) > 0 {
			expiresAt := time.Unix(sub.Items.Data[0].CurrentPeriodEnd, 0)
			proExpiresAt = sql.NullTime{Time: expiresAt, Valid: true}
			s.logger.Info("subscription active",
				"user_id", userID,
				"subscription_id", sub.ID,
				"status", sub.Status,
				"expires_at", expiresAt)
		} else {
			// No items found - this shouldn't happen but handle gracefully
			s.logger.Warn("subscription active but has no items",
				"user_id", userID,
				"subscription_id", sub.ID,
				"status", sub.Status)
			proExpiresAt = sql.NullTime{Valid: false}
		}
	} else {
		// For past_due, canceled, unpaid, etc. - revoke access
		proExpiresAt = sql.NullTime{Valid: false}
		s.logger.Info("subscription inactive",
			"user_id", userID,
			"subscription_id", sub.ID,
			"status", sub.Status)
	}

	if err := s.queries.UpsertEntitlement(ctx, pgdb.UpsertEntitlementParams{
		UserID:               userID,
		ProExpiresAt:         proExpiresAt,
		SubscriptionProvider: sql.NullString{String: "stripe", Valid: true},
	}); err != nil {
		return fmt.Errorf("failed to update entitlement: %w", err)
	}

	return nil
}
