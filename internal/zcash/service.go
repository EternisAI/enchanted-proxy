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
	queries pgdb.Querier
	logger  *logger.Logger
	client  *http.Client
}

func NewService(queries pgdb.Querier, logger *logger.Logger) *Service {
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
		queries: queries,
		logger:  logger,
		client:  client,
	}
}

type CreateInvoiceRequest struct {
	UserID    string `json:"user_id"`
	ProductID string `json:"product_id"`
	AmountZat int64  `json:"amount_zatoshis"`
}

type Invoice struct {
	ID         string    `json:"id"`
	UserID     string    `json:"user_id"`
	ProductID  string    `json:"product_id"`
	AmountZat  int64     `json:"amount_zatoshis"`
	Paid       bool      `json:"paid"`
	Processing bool      `json:"processing"`
	Confirmed  bool      `json:"confirmed"`
	Address    *string   `json:"receiving_address,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
	PaidAt     time.Time `json:"paid_at"`
}

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

	resp, err := s.client.Do(req)
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

func (s *Service) CreateInvoice(ctx context.Context, userID, productID string) (*Invoice, float64, error) {
	apiKey := config.AppConfig.ZCashBackendAPIKey

	product := s.GetProduct(productID)
	if product == nil {
		return nil, 0, fmt.Errorf("unknown product: %s", productID)
	}

	zecPriceUSD, err := s.GetZecPriceUSD(ctx)
	if err != nil {
		return nil, 0, fmt.Errorf("failed to get ZEC price: %w", err)
	}

	priceUSD := float64(product.PriceUSD)

	zecAmount := priceUSD / zecPriceUSD
	zatAmount := int64(zecAmount * 100_000_000)

	reqBody := CreateInvoiceRequest{
		UserID:    userID,
		ProductID: productID,
		AmountZat: zatAmount,
	}

	jsonBody, err := json.Marshal(reqBody)
	if err != nil {
		return nil, 0, fmt.Errorf("failed to marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", config.AppConfig.ZCashBackendURL+"/invoices", bytes.NewReader(jsonBody))
	if err != nil {
		return nil, 0, fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("failed to call zcash backend: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, 0, fmt.Errorf("zcash backend returned status %d", resp.StatusCode)
	}

	var invoice Invoice
	if err := json.NewDecoder(resp.Body).Decode(&invoice); err != nil {
		return nil, 0, fmt.Errorf("failed to decode response: %w", err)
	}

	return &invoice, zecPriceUSD, nil
}

func (s *Service) GetInvoice(ctx context.Context, invoiceID string) (*Invoice, error) {
	apiKey := config.AppConfig.ZCashBackendAPIKey

	req, err := http.NewRequestWithContext(ctx, "GET", config.AppConfig.ZCashBackendURL+"/invoices/"+invoiceID, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to call zcash backend: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, errors.New("invoice not found")
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("zcash backend returned status %d", resp.StatusCode)
	}

	var invoice Invoice
	if err := json.NewDecoder(resp.Body).Decode(&invoice); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	return &invoice, nil
}

func (s *Service) ConfirmPayment(ctx context.Context, userID, invoiceID string) error {
	apiKey := config.AppConfig.ZCashBackendAPIKey

	invoice, err := s.GetInvoice(ctx, invoiceID)
	if err != nil {
		return fmt.Errorf("failed to get invoice: %w", err)
	}

	if !invoice.Paid {
		return errors.New("invoice not paid yet")
	}

	if invoice.Confirmed {
		return errors.New("invoice is already confirmed")
	}

	if invoice.UserID != userID {
		return errors.New("invoice does not belong to user")
	}

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
		expiresAt = sql.NullTime{
			Time:  invoice.UpdatedAt.Add(duration),
			Valid: true,
		}
	}

	err = s.queries.UpsertEntitlementWithTier(ctx, pgdb.UpsertEntitlementWithTierParams{
		UserID:                userID,
		SubscriptionTier:      product.Tier,
		SubscriptionExpiresAt: expiresAt,
		SubscriptionProvider:  "zcash",
		StripeCustomerID:      nil,
	})
	if err != nil {
		return fmt.Errorf("failed to update entitlement: %w", err)
	}

	s.logger.Info("user entitlement updated",
		"user_id", userID,
		"invoice_id", invoiceID,
		"product_id", invoice.ProductID,
		"tier", product.Tier,
		"is_lifetime", product.IsLifetime,
		"expires_at", expiresAt,
	)

	req, err := http.NewRequestWithContext(ctx, "PATCH", config.AppConfig.ZCashBackendURL+"/invoices/"+invoiceID+"/confirmed", nil)
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}

	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to call zcash backend: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("zcash backend returned status %d", resp.StatusCode)
	}

	s.logger.Info("zcash payment confirmed",
		"user_id", userID,
		"invoice_id", invoiceID,
	)

	return nil
}
