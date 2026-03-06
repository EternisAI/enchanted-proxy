package fai

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/google/uuid"

	"github.com/eternisai/enchanted-proxy/internal/config"
	"github.com/eternisai/enchanted-proxy/internal/logger"
	pgdb "github.com/eternisai/enchanted-proxy/internal/storage/pg/sqlc"
	"github.com/eternisai/enchanted-proxy/internal/tiers"
)

var (
	ErrPaymentNotFound = errors.New("payment intent not found")
	ErrInvalidProduct  = errors.New("invalid product")
)

// Service handles FAI crypto payments via the Payment Router contract on Base.
type Service struct {
	queries         pgdb.Querier
	logger          *logger.Logger
	coingecko       *CoinGeckoService
	ethClient       *ethclient.Client
	contractAddress common.Address
	eventSignature  common.Hash
	tokenConfigs    map[string]TokenConfig
	chainID         uint64
}

// Product represents a purchasable subscription product.
type Product struct {
	ID          string  `json:"id"`
	Name        string  `json:"name"`
	Description string  `json:"description"`
	PriceUSD    float64 `json:"price_usd"`
	Tier        string  `json:"tier"`
	IsLifetime  bool    `json:"is_lifetime"`
}

// PaymentIntentResponse is returned when creating a payment intent.
type PaymentIntentResponse struct {
	PaymentID    string  `json:"payment_id"`
	ProductID    string  `json:"product_id"`
	PriceUSD     float64 `json:"price_usd"`
	FaiPrice     float64 `json:"fai_price"`
	FaiAmount    float64 `json:"fai_amount"`
	FaiAmountWei string  `json:"fai_amount_wei"`
	PaymentIDHex string  `json:"payment_id_hex"`
	Status       string  `json:"status"`
}

// ConfigResponse is returned by the config endpoint.
type ConfigResponse struct {
	PaymentContractAddress string `json:"payment_contract_address"`
	FaiTokenAddress        string `json:"fai_token_address"`
	ChainID                uint64 `json:"chain_id"`
}

const (
	ProductWeeklyPro    = "silo.pro.weekly"
	ProductMonthlyPro   = "silo.pro.monthly"
	ProductYearlyPro    = "silo.pro.yearly"
	ProductLifetimePlus = "silo.plus.lifetime"

	PriceWeeklyProUSD    = 4.99
	PriceMonthlyProUSD   = 19.99
	PriceYearlyProUSD    = 199.99
	PriceLifetimePlusUSD = 500
)

func NewService(queries pgdb.Querier, logger *logger.Logger) *Service {
	return &Service{
		queries:   queries,
		logger:    logger,
		coingecko: NewCoinGeckoService(logger),
	}
}

// InitBlockchain connects to the blockchain via WebSocket and sets up event listening.
func (s *Service) InitBlockchain(wsRpcURL, contractAddress string) (err error) {
	ethClient, err := ethclient.Dial(wsRpcURL)
	if err != nil {
		return fmt.Errorf("failed to connect to Ethereum WebSocket: %w", err)
	}
	defer func() {
		if err != nil {
			ethClient.Close()
		}
	}()

	contractAddr := common.HexToAddress(contractAddress)
	eventSig := crypto.Keccak256Hash([]byte("PaymentReceived(bytes32,address,uint256,address)"))

	chainID, err := ethClient.ChainID(context.Background())
	if err != nil {
		return fmt.Errorf("failed to get chain ID: %w", err)
	}

	tokenConfigs, err := GetTokenConfigByChainID(chainID.Uint64())
	if err != nil {
		return fmt.Errorf("failed to get token configs: %w", err)
	}

	s.ethClient = ethClient
	s.contractAddress = contractAddr
	s.eventSignature = eventSig
	s.chainID = chainID.Uint64()
	s.tokenConfigs = tokenConfigs

	s.logger.Info("connected to blockchain",
		"contract_address", contractAddress,
		"chain_id", s.chainID)

	return nil
}

// GetProducts returns the available FAI payment products.
func (s *Service) GetProducts() []Product {
	multiplier := 1.0
	if config.AppConfig.FaiDebugMultiplier > 0 {
		multiplier = config.AppConfig.FaiDebugMultiplier
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

// GetProduct returns a product by ID, or nil if not found.
func (s *Service) GetProduct(productID string) *Product {
	for _, p := range s.GetProducts() {
		if p.ID == productID {
			return &p
		}
	}
	return nil
}

// GetConfig returns blockchain configuration for client-side contract interaction.
func (s *Service) GetConfig() (*ConfigResponse, error) {
	if s.ethClient == nil {
		return nil, fmt.Errorf("blockchain not initialized")
	}

	var faiTokenAddress string
	for addr, tc := range s.tokenConfigs {
		if tc.CoingeckoID == "freysa-ai" {
			faiTokenAddress = addr
			break
		}
	}

	return &ConfigResponse{
		PaymentContractAddress: s.contractAddress.Hex(),
		FaiTokenAddress:        faiTokenAddress,
		ChainID:                s.chainID,
	}, nil
}

// CreatePaymentIntent generates a unique payment ID and stores it in the database.
func (s *Service) CreatePaymentIntent(ctx context.Context, userID, productID string) (*PaymentIntentResponse, error) {
	product := s.GetProduct(productID)
	if product == nil {
		return nil, ErrInvalidProduct
	}

	randomBytes := make([]byte, 32)
	if _, err := rand.Read(randomBytes); err != nil {
		return nil, fmt.Errorf("failed to generate random bytes: %w", err)
	}
	hash := crypto.Keccak256(randomBytes)
	paymentID := hex.EncodeToString(hash[:])

	faiPrice, err := s.coingecko.GetAverageFAIPrice(ctx, 5)
	if err != nil {
		return nil, fmt.Errorf("failed to get FAI price: %w", err)
	}
	if faiPrice <= 0 {
		return nil, fmt.Errorf("invalid FAI price: %f", faiPrice)
	}

	id := uuid.New().String()
	err = s.queries.CreateFaiPaymentIntent(ctx, pgdb.CreateFaiPaymentIntentParams{
		ID:        id,
		UserID:    userID,
		PaymentID: paymentID,
		ProductID: productID,
		PriceUsd:  product.PriceUSD,
		FaiPrice:  sql.NullFloat64{Float64: faiPrice, Valid: faiPrice > 0},
	})
	if err != nil {
		return nil, fmt.Errorf("failed to store payment intent: %w", err)
	}

	faiAmount, faiAmountWei := computeFaiAmountFields(product.PriceUSD, faiPrice)

	s.logger.Info("created FAI payment intent",
		"payment_id", paymentID,
		"user_id", userID,
		"product_id", productID,
		"price_usd", product.PriceUSD,
		"fai_price", faiPrice,
		"fai_amount", faiAmount)

	return &PaymentIntentResponse{
		PaymentID:    paymentID,
		ProductID:    productID,
		PriceUSD:     product.PriceUSD,
		FaiPrice:     faiPrice,
		FaiAmount:    faiAmount,
		FaiAmountWei: faiAmountWei,
		PaymentIDHex: "0x" + paymentID,
		Status:       "pending",
	}, nil
}

// GetPaymentStatus returns the current status of a payment intent.
func (s *Service) GetPaymentStatus(ctx context.Context, paymentID, userID string) (*PaymentIntentResponse, error) {
	intent, err := s.queries.GetFaiPaymentIntentForUser(ctx, pgdb.GetFaiPaymentIntentForUserParams{
		PaymentID: paymentID,
		UserID:    userID,
	})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrPaymentNotFound
		}
		return nil, err
	}

	var faiPrice float64
	if intent.FaiPrice.Valid {
		faiPrice = intent.FaiPrice.Float64
	}

	faiAmount, faiAmountWei := computeFaiAmountFields(intent.PriceUsd, faiPrice)

	return &PaymentIntentResponse{
		PaymentID:    intent.PaymentID,
		ProductID:    intent.ProductID,
		PriceUSD:     intent.PriceUsd,
		FaiPrice:     faiPrice,
		FaiAmount:    faiAmount,
		FaiAmountWei: faiAmountWei,
		PaymentIDHex: "0x" + intent.PaymentID,
		Status:       intent.Status,
	}, nil
}

// StartEventListener subscribes to PaymentReceived events from the Payment Router contract.
// This should be run as a goroutine.
func (s *Service) StartEventListener(ctx context.Context) error {
	if s.ethClient == nil {
		return fmt.Errorf("blockchain not initialized - call InitBlockchain first")
	}

	s.logger.Info("starting FAI payment event listener",
		"contract_address", s.contractAddress.Hex(),
		"event_signature", s.eventSignature.Hex())

	blockNumber, err := s.syncHistoricalBlocks(ctx)
	if err != nil {
		s.logger.Warn("failed to sync historical blocks, starting from current block", "error", err.Error())
		blockNumber, err = s.ethClient.BlockNumber(ctx)
		if err != nil {
			return fmt.Errorf("failed to get current block number: %w", err)
		}
	}

	query := ethereum.FilterQuery{
		Addresses: []common.Address{s.contractAddress},
		Topics:    [][]common.Hash{{s.eventSignature}},
		FromBlock: big.NewInt(int64(blockNumber)),
	}

	logs := make(chan types.Log)
	sub, err := s.ethClient.SubscribeFilterLogs(ctx, query, logs)
	if err != nil {
		return fmt.Errorf("failed to subscribe to logs: %w", err)
	}

	s.logger.Info("subscribed to PaymentReceived events")

	for {
		select {
		case err := <-sub.Err():
			s.logger.Error("subscription error, resubscribing", "error", err)
			blockNumber, syncErr := s.syncHistoricalBlocks(ctx)
			if syncErr != nil {
				s.logger.Error("failed to sync historical blocks after error", "error", syncErr.Error())
				return fmt.Errorf("failed to sync historical blocks: %w", syncErr)
			}
			query.FromBlock = big.NewInt(int64(blockNumber))
			sub, err = s.ethClient.SubscribeFilterLogs(ctx, query, logs)
			if err != nil {
				return fmt.Errorf("failed to resubscribe: %w", err)
			}
			s.logger.Info("resubscribed to PaymentReceived events")

		case vLog := <-logs:
			s.logger.Info("received log", "block_number", vLog.BlockNumber, "tx_hash", vLog.TxHash.Hex())
			if err := s.handlePaymentReceivedEvent(ctx, vLog); err != nil {
				s.logger.Error("failed to handle payment event", "error", err.Error())
			}

		case <-ctx.Done():
			s.logger.Info("stopping event listener")
			sub.Unsubscribe()
			return ctx.Err()
		}
	}
}

// handlePaymentReceivedEvent processes a PaymentReceived event from the blockchain.
func (s *Service) handlePaymentReceivedEvent(ctx context.Context, vLog types.Log) error {
	if len(vLog.Topics) == 0 || vLog.Topics[0] != s.eventSignature {
		return nil
	}

	// PaymentReceived(bytes32 paymentId, address token, uint256 amount, address from)
	// Data: paymentId (32 bytes) + token (32 bytes) + amount (32 bytes) + from (32 bytes)
	if len(vLog.Data) < 128 {
		return fmt.Errorf("insufficient event data: expected 128 bytes, got %d", len(vLog.Data))
	}

	var paymentIDBytes [32]byte
	copy(paymentIDBytes[:], vLog.Data[0:32])
	paymentIDHex := hex.EncodeToString(paymentIDBytes[:])

	tokenAddress := common.BytesToAddress(vLog.Data[44:64])
	amount := new(big.Int).SetBytes(vLog.Data[64:96])
	fromAddress := common.BytesToAddress(vLog.Data[108:128])

	s.logger.Info("received payment event",
		"payment_id", paymentIDHex,
		"token_address", tokenAddress.Hex(),
		"amount", amount.String(),
		"from_address", fromAddress.Hex(),
		"tx_hash", vLog.TxHash.Hex())

	intent, err := s.queries.GetFaiPaymentIntentByPaymentID(ctx, paymentIDHex)
	if errors.Is(err, sql.ErrNoRows) {
		s.logger.Info("payment ID not found in database (may belong to another app)", "payment_id", paymentIDHex)
		return nil
	}
	if err != nil {
		return fmt.Errorf("failed to query payment intent: %w", err)
	}

	// Idempotency: skip if not pending (already completed or expired)
	if intent.Status != "pending" {
		s.logger.Info("payment intent not pending, skipping", "payment_id", paymentIDHex, "status", intent.Status)
		return nil
	}

	tokenConfig, exists := s.tokenConfigs[tokenAddress.Hex()]
	if !exists {
		s.logger.Warn("unsupported token", "token_address", tokenAddress.Hex())
		return fmt.Errorf("unsupported token: %s", tokenAddress.Hex())
	}

	// Calculate token amount from wei
	divisor := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(tokenConfig.Decimals)), nil)
	tokenAmountFloat, _ := new(big.Float).Quo(
		new(big.Float).SetInt(amount),
		new(big.Float).SetInt(divisor),
	).Float64()

	// Use the stored FAI price from intent creation for validation to avoid
	// rejecting valid payments due to price changes between creation and payment.
	// Fall back to live CoinGecko price if the stored price is unavailable.
	var tokenPriceUSD float64
	if intent.FaiPrice.Valid && intent.FaiPrice.Float64 > 0 {
		tokenPriceUSD = intent.FaiPrice.Float64
	} else {
		tokenPriceUSD, err = s.coingecko.GetTokenPrice(ctx, tokenConfig.CoingeckoID)
		if err != nil {
			return fmt.Errorf("failed to get token price: %w", err)
		}
	}

	usdValue := tokenAmountFloat * tokenPriceUSD

	s.logger.Info("payment value calculated",
		"payment_id", paymentIDHex,
		"token_amount", tokenAmountFloat,
		"token_price_usd", tokenPriceUSD,
		"usd_value", usdValue)

	// Validate payment amount covers the product price (allow 5% slippage for price volatility)
	product := s.GetProduct(intent.ProductID)
	if product == nil {
		return fmt.Errorf("unknown product: %s", intent.ProductID)
	}

	minRequired := intent.PriceUsd * 0.95
	if usdValue < minRequired {
		s.logger.Warn("payment amount insufficient",
			"payment_id", paymentIDHex,
			"usd_value", usdValue,
			"expected_usd", intent.PriceUsd,
			"min_required", minRequired)
		return fmt.Errorf("payment insufficient: received $%.2f, expected $%.2f", usdValue, intent.PriceUsd)
	}

	if err := s.grantEntitlement(ctx, intent, product); err != nil {
		return fmt.Errorf("failed to grant entitlement: %w", err)
	}

	txHash := vLog.TxHash.Hex()
	tokenAddr := tokenAddress.Hex()
	err = s.queries.UpdateFaiPaymentIntentToCompleted(ctx, pgdb.UpdateFaiPaymentIntentToCompletedParams{
		PaymentID:    paymentIDHex,
		TokenAddress: &tokenAddr,
		TokenAmount:  sql.NullFloat64{Float64: tokenAmountFloat, Valid: true},
		PaidBlock:    sql.NullInt64{Int64: int64(vLog.BlockNumber), Valid: true},
		TxHash:       &txHash,
	})
	if err != nil {
		s.logger.Error("failed to update payment intent status", "error", err.Error(), "payment_id", paymentIDHex)
		// Don't return error — entitlement was already granted
	}

	s.logger.Info("payment processed successfully",
		"payment_id", paymentIDHex,
		"user_id", intent.UserID,
		"product_id", intent.ProductID,
		"token_address", tokenAddress.Hex(),
		"usd_value", usdValue,
		"block", vLog.BlockNumber)

	return nil
}

// grantEntitlement grants the user a subscription based on the product purchased.
func (s *Service) grantEntitlement(ctx context.Context, intent pgdb.FaiPaymentIntent, product *Product) error {
	if product.IsLifetime {
		err := s.queries.UpsertEntitlementWithTier(ctx, pgdb.UpsertEntitlementWithTierParams{
			UserID:           intent.UserID,
			SubscriptionTier: product.Tier,
			SubscriptionExpiresAt: sql.NullTime{
				Time:  parseTime("2099-12-31T23:59:59Z"),
				Valid: true,
			},
			SubscriptionProvider: "fai",
			StripeCustomerID:     nil,
		})
		if err != nil {
			return err
		}
		s.logger.Info("lifetime entitlement granted", "user_id", intent.UserID, "tier", product.Tier)
		return nil
	}

	var durationDays int32
	switch product.ID {
	case ProductWeeklyPro:
		durationDays = 7
	case ProductMonthlyPro:
		durationDays = 30
	case ProductYearlyPro:
		durationDays = 365
	default:
		durationDays = 30
	}

	baseTime := time.Now()
	if intent.PaidAt.Valid {
		baseTime = intent.PaidAt.Time
	}

	err := s.queries.UpsertEntitlementWithExtension(ctx, pgdb.UpsertEntitlementWithExtensionParams{
		UserID:               intent.UserID,
		SubscriptionTier:     product.Tier,
		BaseTime:             baseTime,
		DurationDays:         durationDays,
		SubscriptionProvider: "fai",
		StripeCustomerID:     nil,
	})
	if err != nil {
		return err
	}

	s.logger.Info("entitlement granted/extended",
		"user_id", intent.UserID,
		"tier", product.Tier,
		"duration_days", durationDays)

	return nil
}

// syncHistoricalBlocks catches up on any missed PaymentReceived events.
func (s *Service) syncHistoricalBlocks(ctx context.Context) (uint64, error) {
	currentBlock, err := s.ethClient.BlockNumber(ctx)
	if err != nil {
		return 0, fmt.Errorf("failed to get current block number: %w", err)
	}

	// Look back ~1 hour on Base (≈ 1800 blocks)
	fromBlock := currentBlock
	if currentBlock > 1800 {
		fromBlock = currentBlock - 1800
	}

	s.logger.Info("syncing historical blocks", "from_block", fromBlock, "to_block", currentBlock)

	const batchSize = uint64(500)
	for start := fromBlock; start <= currentBlock; start += batchSize {
		end := start + batchSize - 1
		if end > currentBlock {
			end = currentBlock
		}

		query := ethereum.FilterQuery{
			FromBlock: big.NewInt(int64(start)),
			ToBlock:   big.NewInt(int64(end)),
			Addresses: []common.Address{s.contractAddress},
			Topics:    [][]common.Hash{{s.eventSignature}},
		}

		historicalLogs, err := s.ethClient.FilterLogs(ctx, query)
		if err != nil {
			return 0, fmt.Errorf("failed to filter logs: %w", err)
		}

		for _, vLog := range historicalLogs {
			if err := s.handlePaymentReceivedEvent(ctx, vLog); err != nil {
				s.logger.Error("failed to handle historical event", "error", err.Error(), "block", vLog.BlockNumber)
			}
		}
	}

	s.logger.Info("historical block sync completed", "synced_to_block", currentBlock)
	return currentBlock, nil
}

// Close cleans up the blockchain connection.
func (s *Service) Close() {
	if s.ethClient != nil {
		s.ethClient.Close()
		s.logger.Info("closed Ethereum WebSocket connection")
	}
}

// computeFaiAmountFields computes the FAI token amount and its wei representation.
func computeFaiAmountFields(priceUSD, faiUSDPrice float64) (faiAmount float64, faiAmountWei string) {
	if faiUSDPrice <= 0 {
		return 0, ""
	}
	faiAmount = priceUSD / faiUSDPrice
	bigAmount := new(big.Float).SetFloat64(faiAmount)
	weiFloat := new(big.Float).Mul(bigAmount, new(big.Float).SetFloat64(1e18))
	weiInt, _ := weiFloat.Int(nil)
	faiAmountWei = "0x" + weiInt.Text(16)
	return faiAmount, faiAmountWei
}

func parseTime(s string) (t time.Time) {
	t, _ = time.Parse(time.RFC3339, s)
	return
}
