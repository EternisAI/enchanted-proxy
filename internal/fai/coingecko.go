package fai

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/eternisai/enchanted-proxy/internal/logger"
)

// CoinGeckoService fetches token prices from CoinGecko.
type CoinGeckoService struct {
	logger  *logger.Logger
	client  *http.Client
	baseURL string
}

func NewCoinGeckoService(logger *logger.Logger) *CoinGeckoService {
	return &CoinGeckoService{
		logger:  logger,
		client:  &http.Client{Timeout: 30 * time.Second},
		baseURL: "https://api.coingecko.com/api/v3",
	}
}

// GetTokenPrice fetches the current USD price for a token by its CoinGecko ID.
func (c *CoinGeckoService) GetTokenPrice(ctx context.Context, tokenID string) (float64, error) {
	reqURL := fmt.Sprintf("%s/simple/price?ids=%s&vs_currencies=usd", c.baseURL, url.QueryEscape(tokenID))

	req, err := http.NewRequestWithContext(ctx, "GET", reqURL, nil)
	if err != nil {
		return 0, fmt.Errorf("failed to create request: %w", err)
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("failed to make request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return 0, fmt.Errorf("CoinGecko API returned status %d: %s", resp.StatusCode, string(body))
	}

	var priceData map[string]map[string]float64
	if err := json.NewDecoder(resp.Body).Decode(&priceData); err != nil {
		return 0, fmt.Errorf("failed to decode response: %w", err)
	}

	tokenData, exists := priceData[tokenID]
	if !exists {
		return 0, fmt.Errorf("token data not found for token ID: %s", tokenID)
	}

	price, exists := tokenData["usd"]
	if !exists {
		return 0, fmt.Errorf("USD price not found for token ID: %s", tokenID)
	}

	c.logger.Info("fetched token price", "token_id", tokenID, "price_usd", price)
	return price, nil
}

// GetAverageFAIPrice fetches the average FAI price over the given number of minutes.
func (c *CoinGeckoService) GetAverageFAIPrice(ctx context.Context, minutes int) (float64, error) {
	if minutes <= 0 {
		return 0, fmt.Errorf("minutes must be positive")
	}

	toTime := time.Now().Unix()
	fromTime := time.Now().Add(-time.Duration(minutes) * time.Minute).Unix()

	url := fmt.Sprintf("%s/coins/freysa-ai/market_chart/range?vs_currency=usd&from=%d&to=%d",
		c.baseURL, fromTime, toTime)

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return 0, fmt.Errorf("failed to create request: %w", err)
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("failed to make request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return 0, fmt.Errorf("CoinGecko API returned status %d: %s", resp.StatusCode, string(body))
	}

	var histData struct {
		Prices [][]float64 `json:"prices"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&histData); err != nil {
		return 0, fmt.Errorf("failed to decode response: %w", err)
	}

	if len(histData.Prices) == 0 {
		// Fall back to current price
		return c.GetTokenPrice(ctx, "freysa-ai")
	}

	var sum float64
	var validCount int
	for _, pricePoint := range histData.Prices {
		if len(pricePoint) >= 2 {
			sum += pricePoint[1]
			validCount++
		}
	}

	if validCount == 0 {
		c.logger.Info("no valid price points in historical data, falling back to current price")
		return c.GetTokenPrice(ctx, "freysa-ai")
	}

	average := sum / float64(validCount)
	c.logger.Info("calculated average FAI price", "average_price", average, "data_points", validCount)
	return average, nil
}
