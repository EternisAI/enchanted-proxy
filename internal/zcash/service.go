package zcash

import (
	"bytes"
	"context"
	"crypto/tls"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"cloud.google.com/go/firestore"
	"github.com/google/uuid"

	"github.com/eternisai/enchanted-proxy/internal/config"
	"github.com/eternisai/enchanted-proxy/internal/logger"
	pgdb "github.com/eternisai/enchanted-proxy/internal/storage/pg/sqlc"
	"github.com/eternisai/enchanted-proxy/internal/tiers"
)

const (
	krakenAPIURL = "https://api.kraken.com/0/public/Ticker?pair=ZECUSD"

	ProductWeeklyPro    = "silo.pro.weekly"
	ProductMonthlyPro   = "silo.pro.monthly"
	ProductYearlyPro    = "silo.pro.yearly"
	ProductLifetimePlus = "silo.plus.lifetime"

	PriceWeeklyProUSD    = 4.99
	PriceMonthlyProUSD   = 19.99
	PriceYearlyProUSD    = 199.99
	PriceLifetimePlusUSD = 500
)

type Service struct {
	queries         pgdb.Querier
	logger          *logger.Logger
	httpClient      *http.Client
	firestoreClient *firestore.Client
}

func NewService(queries pgdb.Querier, firestoreClient *firestore.Client, logger *logger.Logger) *Service {
	client := &http.Client{
		Timeout: 60 * time.Second,
	}

	if config.AppConfig.ZCashBackendSkipTLSVerify {
		client.Transport = &http.Transport{
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: true,
			},
		}
		logger.Warn("zcash backend TLS verification disabled (dev only)")
	}

	return &Service{
		queries:         queries,
		logger:          logger,
		httpClient:      client,
		firestoreClient: firestoreClient,
	}
}

// Invoice represents an invoice stored in the local database.
type Invoice struct {
	ID               uuid.UUID
	UserID           string
	ProductID        string
	AmountZatoshis   int64
	ZecAmount        float64
	PriceUSD         float64
	ReceivingAddress string
	Status           string
	CreatedAt        time.Time
	UpdatedAt        time.Time
	PaidAt           *time.Time
}

// Product represents a purchasable product.
type Product struct {
	ID          string  `json:"id"`
	Name        string  `json:"name"`
	Description string  `json:"description"`
	PriceUSD    float64 `json:"price_usd"`
	Tier        string  `json:"tier"`
	IsLifetime  bool    `json:"is_lifetime"`
}

func (s *Service) GetProducts() []Product {
	multiplier := 1.0
	if config.AppConfig.ZCashDebugMultiplier > 0 {
		multiplier = config.AppConfig.ZCashDebugMultiplier
	}
	return []Product{
		{
			ID:          ProductWeeklyPro,
			Name:        "Pro Weekly",
			Description: "Pro subscription for 1 week",
			PriceUSD:    PriceWeeklyProUSD * multiplier,
			Tier:        string(tiers.TierPro),
			IsLifetime:  false,
		},
		{
			ID:          ProductMonthlyPro,
			Name:        "Pro Monthly",
			Description: "Pro subscription for 1 month",
			PriceUSD:    PriceMonthlyProUSD * multiplier,
			Tier:        string(tiers.TierPro),
			IsLifetime:  false,
		},
		{
			ID:          ProductYearlyPro,
			Name:        "Pro Yearly",
			Description: "Pro subscription for 1 year",
			PriceUSD:    PriceYearlyProUSD * multiplier,
			Tier:        string(tiers.TierPro),
			IsLifetime:  false,
		},
		{
			ID:          ProductLifetimePlus,
			Name:        "Plus Lifetime",
			Description: "Plus subscription forever",
			PriceUSD:    PriceLifetimePlusUSD * multiplier,
			Tier:        string(tiers.TierPlus),
			IsLifetime:  true,
		},
	}
}

func (s *Service) GetProduct(productID string) *Product {
	for _, p := range s.GetProducts() {
		if p.ID == productID {
			return &p
		}
	}
	return nil
}

type KrakenTickerResponse struct {
	Error  []string                    `json:"error"`
	Result map[string]KrakenTickerData `json:"result"`
}

type KrakenTickerData struct {
	LastTrade []string `json:"c"` // c = last trade closed [price, lot volume]
}

func (s *Service) GetZecPriceUSD(ctx context.Context) (float64, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", krakenAPIURL, nil)
	if err != nil {
		return 0, fmt.Errorf("failed to create kraken request: %w", err)
	}

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return 0, fmt.Errorf("failed to call kraken API: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("kraken API returned status %d", resp.StatusCode)
	}

	var result KrakenTickerResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return 0, fmt.Errorf("failed to decode kraken response: %w", err)
	}

	if len(result.Error) > 0 {
		return 0, fmt.Errorf("kraken API error: %s", result.Error[0])
	}

	// Kraken uses X-prefixed asset names (XZEC for Zcash)
	tickerData, ok := result.Result["XZECZUSD"]
	if !ok {
		return 0, errors.New("XZECZUSD pair not found in kraken response")
	}

	if len(tickerData.LastTrade) == 0 {
		return 0, errors.New("no last trade data in kraken response")
	}

	price, err := strconv.ParseFloat(tickerData.LastTrade[0], 64)
	if err != nil {
		return 0, fmt.Errorf("failed to parse price: %w", err)
	}

	return price, nil
}

// CreateInvoice creates a new invoice, stores it locally, writes to Firestore,
// and calls the zcash-backend to get a receiving address.
func (s *Service) CreateInvoice(ctx context.Context, userID, productID string) (*Invoice, error) {
	product := s.GetProduct(productID)
	if product == nil {
		return nil, fmt.Errorf("unknown product: %s", productID)
	}

	// Get ZEC price from Kraken
	zecPriceUSD, err := s.GetZecPriceUSD(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get ZEC price: %w", err)
	}

	// Calculate amounts
	zecAmount := product.PriceUSD / zecPriceUSD
	zatAmount := int64(zecAmount * 100_000_000)

	// Generate invoice ID
	invoiceID := uuid.New()

	// Call zcash-backend to create invoice and get address
	address, err := s.createBackendInvoice(ctx, invoiceID.String(), userID, productID, zatAmount)
	if err != nil {
		return nil, fmt.Errorf("failed to create backend invoice: %w", err)
	}

	// Store in local database
	err = s.queries.CreateZcashInvoice(ctx, pgdb.CreateZcashInvoiceParams{
		ID:               invoiceID,
		UserID:           userID,
		ProductID:        productID,
		AmountZatoshis:   zatAmount,
		ZecAmount:        zecAmount,
		PriceUsd:         product.PriceUSD,
		ReceivingAddress: address,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to store invoice: %w", err)
	}

	// Write to Firestore for real-time client updates
	now := time.Now()
	firestoreData := &ZcashInvoiceFirestore{
		UserID:           userID,
		ProductID:        productID,
		AmountZatoshis:   zatAmount,
		ZecAmount:        zecAmount,
		PriceUSD:         product.PriceUSD,
		ReceivingAddress: address,
		Status:           "pending",
		CreatedAt:        now,
		UpdatedAt:        now,
	}
	if err := s.WriteInvoiceToFirestore(ctx, invoiceID.String(), firestoreData); err != nil {
		s.logger.Error("failed to write invoice to Firestore", "error", err.Error(), "invoice_id", invoiceID.String())
	}

	s.logger.Info("zcash invoice created",
		"invoice_id", invoiceID.String(),
		"user_id", userID,
		"product_id", productID,
		"zec_amount", zecAmount,
		"zat_amount", zatAmount,
	)

	return &Invoice{
		ID:               invoiceID,
		UserID:           userID,
		ProductID:        productID,
		AmountZatoshis:   zatAmount,
		ZecAmount:        zecAmount,
		PriceUSD:         product.PriceUSD,
		ReceivingAddress: address,
		Status:           "pending",
		CreatedAt:        now,
		UpdatedAt:        now,
	}, nil
}

// GetInvoiceForUser retrieves an invoice, verifying it belongs to the user.
func (s *Service) GetInvoiceForUser(ctx context.Context, invoiceIDStr, userID string) (*Invoice, error) {
	invoiceID, err := uuid.Parse(invoiceIDStr)
	if err != nil {
		return nil, fmt.Errorf("invalid invoice ID: %w", err)
	}

	row, err := s.queries.GetZcashInvoiceForUser(ctx, pgdb.GetZcashInvoiceForUserParams{
		ID:     invoiceID,
		UserID: userID,
	})
	if err != nil {
		return nil, err
	}

	var paidAt *time.Time
	if row.PaidAt.Valid {
		paidAt = &row.PaidAt.Time
	}

	return &Invoice{
		ID:               row.ID,
		UserID:           row.UserID,
		ProductID:        row.ProductID,
		AmountZatoshis:   row.AmountZatoshis,
		ZecAmount:        row.ZecAmount,
		PriceUSD:         row.PriceUsd,
		ReceivingAddress: row.ReceivingAddress,
		Status:           row.Status,
		CreatedAt:        row.CreatedAt,
		UpdatedAt:        row.UpdatedAt,
		PaidAt:           paidAt,
	}, nil
}

// HandlePaymentCallback processes a callback from zcash-payment-backend.
func (s *Service) HandlePaymentCallback(ctx context.Context, invoiceIDStr, status string, accumulatedZatoshis int64) error {
	invoiceID, err := uuid.Parse(invoiceIDStr)
	if err != nil {
		return fmt.Errorf("invalid invoice ID: %w", err)
	}

	// Get invoice from local DB
	row, err := s.queries.GetZcashInvoice(ctx, invoiceID)
	if err != nil {
		return fmt.Errorf("invoice not found: %w", err)
	}

	// Idempotency: if already paid, just return success
	if row.Status == "paid" {
		s.logger.Debug("invoice already paid, ignoring callback", "invoice_id", invoiceIDStr)
		return nil
	}

	// Validate status
	if status != "processing" && status != "paid" {
		return fmt.Errorf("invalid status: %s", status)
	}

	// Validate payment amount before marking as paid
	if status == "paid" && accumulatedZatoshis < row.AmountZatoshis {
		return fmt.Errorf("insufficient payment: got %d zatoshis, expected %d", accumulatedZatoshis, row.AmountZatoshis)
	}

	// If payment complete, grant entitlement
	if status == "paid" {
		if err := s.grantEntitlement(ctx, row); err != nil {
			return fmt.Errorf("failed to grant entitlement: %w", err)
		}
	}

	// Update local DB
	if status == "processing" {
		err = s.queries.UpdateZcashInvoiceToProcessing(ctx, invoiceID)
	} else {
		err = s.queries.UpdateZcashInvoiceToPaid(ctx, invoiceID)
	}
	if err != nil {
		return fmt.Errorf("failed to update invoice status: %w", err)
	}

	// Update Firestore
	if err := s.UpdateInvoiceStatusInFirestore(ctx, invoiceIDStr, status); err != nil {
		s.logger.Error("failed to update Firestore", "error", err.Error(), "invoice_id", invoiceIDStr)
	}

	s.logger.Info("zcash payment callback processed",
		"invoice_id", invoiceIDStr,
		"status", status,
		"accumulated_zatoshis", accumulatedZatoshis,
	)

	return nil
}

func (s *Service) grantEntitlement(ctx context.Context, invoice pgdb.ZcashInvoice) error {
	product := s.GetProduct(invoice.ProductID)
	if product == nil {
		return fmt.Errorf("unknown product: %s", invoice.ProductID)
	}

	var expiresAt sql.NullTime
	if product.IsLifetime {
		expiresAt = sql.NullTime{
			Time:  time.Date(2099, 12, 31, 23, 59, 59, 0, time.UTC),
			Valid: true,
		}
	} else {
		var duration time.Duration
		switch product.ID {
		case ProductWeeklyPro:
			duration = 7 * 24 * time.Hour
		case ProductMonthlyPro:
			duration = 30 * 24 * time.Hour
		case ProductYearlyPro:
			duration = 365 * 24 * time.Hour
		default:
			duration = 30 * 24 * time.Hour
		}
		// Use invoice.CreatedAt as base for stable expiration calculation
		// This prevents race conditions where duplicate callbacks would calculate different expirations
		expiresAt = sql.NullTime{
			Time:  invoice.CreatedAt.Add(duration),
			Valid: true,
		}
	}

	err := s.queries.UpsertEntitlementWithTier(ctx, pgdb.UpsertEntitlementWithTierParams{
		UserID:                invoice.UserID,
		SubscriptionTier:      product.Tier,
		SubscriptionExpiresAt: expiresAt,
		SubscriptionProvider:  "zcash",
		StripeCustomerID:      nil,
	})
	if err != nil {
		return err
	}

	s.logger.Info("entitlement granted",
		"user_id", invoice.UserID,
		"invoice_id", invoice.ID.String(),
		"tier", product.Tier,
		"expires_at", expiresAt.Time,
	)

	return nil
}

// createBackendInvoice calls zcash-backend to create invoice and get receiving address.
func (s *Service) createBackendInvoice(ctx context.Context, invoiceID, userID, productID string, zatAmount int64) (string, error) {
	backendURL := config.AppConfig.ZCashBackendURL + "/invoices"
	apiKey := config.AppConfig.ZCashBackendAPIKey

	reqBody := map[string]any{
		"id":              invoiceID,
		"user_id":         userID,
		"product_id":      productID,
		"amount_zatoshis": zatAmount,
	}

	reqJSON, err := json.Marshal(reqBody)
	if err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", backendURL, bytes.NewBuffer(reqJSON))
	if err != nil {
		return "", err
	}

	req.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return "", fmt.Errorf("backend returned status %d", resp.StatusCode)
	}

	var result struct {
		ID               string `json:"id"`
		ReceivingAddress string `json:"receiving_address"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}

	return result.ReceivingAddress, nil
}
