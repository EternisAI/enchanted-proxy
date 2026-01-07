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
	"strings"
	"time"

	"github.com/eternisai/enchanted-proxy/internal/config"
	"github.com/eternisai/enchanted-proxy/internal/logger"
	pgdb "github.com/eternisai/enchanted-proxy/internal/storage/pg/sqlc"
	"github.com/eternisai/enchanted-proxy/internal/tiers"
)

const (
	krakenAPIURL = "https://api.kraken.com/0/public/Ticker?pair=ZECUSD"

	ProductMonthlyPro   = "monthly_pro"
	ProductLifetimePlus = "lifetime_plus"

	PriceMonthlyProUSD   = 20
	PriceLifetimePlusUSD = 500
)

type Service struct {
	queries pgdb.Querier
	logger  *logger.Logger
	client  *http.Client
}

func NewService(queries pgdb.Querier, logger *logger.Logger) *Service {
	client := &http.Client{
		Timeout: 30 * time.Second,
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
	InvoiceID   string `json:"invoice_id,omitempty"`
	ExpectedZat int64  `json:"expected_zat,omitempty"`
}

type CreateInvoiceResponse struct {
	InvoiceID string `json:"invoice_id"`
	Address   string `json:"address"`
}

type InvoiceStatusResponse struct {
	InvoiceID   string  `json:"invoice_id"`
	Address     string  `json:"address"`
	ExpectedZat *int64  `json:"expected_zat,omitempty"`
	Status      string  `json:"status"` // "paid" | "unpaid"
	PaidZat     *int64  `json:"paid_zat,omitempty"`
	PaidTxID    *string `json:"paid_txid,omitempty"`
	PaidHeight  *int32  `json:"paid_height,omitempty"`
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
			ID:          ProductMonthlyPro,
			Name:        "Pro Monthly",
			Description: "Pro subscription for 1 month",
			PriceUSD:    float64(PriceMonthlyProUSD) * multiplier,
			Tier:        string(tiers.TierPro),
			IsLifetime:  false,
		},
		{
			ID:          ProductLifetimePlus,
			Name:        "Lifetime Plus",
			Description: "Plus subscription forever",
			PriceUSD:    float64(PriceLifetimePlusUSD) * multiplier,
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
		return 0, fmt.Errorf("XZECZUSD pair not found in kraken response")
	}

	if len(tickerData.LastTrade) == 0 {
		return 0, fmt.Errorf("no last trade data in kraken response")
	}

	price, err := strconv.ParseFloat(tickerData.LastTrade[0], 64)
	if err != nil {
		return 0, fmt.Errorf("failed to parse price: %w", err)
	}

	return price, nil
}

func (s *Service) CreateInvoice(ctx context.Context, userID, productID string) (*CreateInvoiceResponse, float64, error) {
	apiKey := config.AppConfig.ZCashBackendAPIKey
	if apiKey == "" {
		return nil, 0, errors.New("zcash backend API key not configured")
	}

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

	invoiceID := fmt.Sprintf("%s__%s__%d", userID, productID, time.Now().Unix())

	reqBody := CreateInvoiceRequest{
		InvoiceID:   invoiceID,
		ExpectedZat: zatAmount,
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
	req.Header.Set("Authorization", "Bearer "+apiKey)

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("failed to call zcash backend: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, 0, fmt.Errorf("zcash backend returned status %d", resp.StatusCode)
	}

	var result CreateInvoiceResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, 0, fmt.Errorf("failed to decode response: %w", err)
	}

	return &result, zecPriceUSD, nil
}

func (s *Service) GetInvoiceStatus(ctx context.Context, invoiceID string) (*InvoiceStatusResponse, error) {
	apiKey := config.AppConfig.ZCashBackendAPIKey
	if apiKey == "" {
		return nil, errors.New("zcash backend API key not configured")
	}

	req, err := http.NewRequestWithContext(ctx, "GET", config.AppConfig.ZCashBackendURL+"/invoices/"+invoiceID, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to call zcash backend: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("invoice not found")
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("zcash backend returned status %d", resp.StatusCode)
	}

	var result InvoiceStatusResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	return &result, nil
}

func (s *Service) ConfirmPayment(ctx context.Context, userID, invoiceID string) error {
	status, err := s.GetInvoiceStatus(ctx, invoiceID)
	if err != nil {
		return fmt.Errorf("failed to get invoice status: %w", err)
	}

	if status.Status != "paid" {
		return fmt.Errorf("invoice not paid yet")
	}

	parts := parseInvoiceID(invoiceID)
	if parts == nil {
		return fmt.Errorf("invalid invoice ID format")
	}

	if parts.userID != userID {
		return fmt.Errorf("invoice does not belong to user")
	}

	product := s.GetProduct(parts.productID)
	if product == nil {
		return fmt.Errorf("unknown product: %s", parts.productID)
	}

	var expiresAt sql.NullTime
	if product.IsLifetime {
		expiresAt = sql.NullTime{
			Time:  time.Date(2099, 12, 31, 23, 59, 59, 0, time.UTC),
			Valid: true,
		}
	} else {
		expiresAt = sql.NullTime{
			Time:  time.Now().Add(30 * 24 * time.Hour),
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

	s.logger.Info("zcash payment confirmed",
		"user_id", userID,
		"invoice_id", invoiceID,
		"product_id", parts.productID,
		"tier", product.Tier,
		"is_lifetime", product.IsLifetime,
	)

	return nil
}

type invoiceParts struct {
	userID    string
	productID string
	timestamp string
}

func parseInvoiceID(invoiceID string) *invoiceParts {
	parts := strings.SplitN(invoiceID, "__", 3)
	if len(parts) != 3 {
		return nil
	}
	return &invoiceParts{
		userID:    parts[0],
		productID: parts[1],
		timestamp: parts[2],
	}
}
