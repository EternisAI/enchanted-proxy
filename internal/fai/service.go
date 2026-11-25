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

// chainReader is the part of the chain client the listener uses: subscribing to
// events, replaying them and dating payments. *ethclient.Client implements it;
// tests use a fake.
type chainReader interface {
	BlockNumber(ctx context.Context) (uint64, error)
	FilterLogs(ctx context.Context, q ethereum.FilterQuery) ([]types.Log, error)
	HeaderByNumber(ctx context.Context, number *big.Int) (*types.Header, error)
	SubscribeFilterLogs(ctx context.Context, q ethereum.FilterQuery, ch chan<- types.Log) (ethereum.Subscription, error)
}

// blockRange is an inclusive range of blocks.
type blockRange struct{ from, to uint64 }

// Service handles FAI crypto payments via the Payment Router contract on Base.
type Service struct {
	db              *sql.DB
	queries         pgdb.Querier
	logger          *logger.Logger
	coingecko       *CoinGeckoService
	ethClient       *ethclient.Client
	contractAddress common.Address
	eventSignature  common.Hash
	tokenConfigs    map[string]TokenConfig
	chainID         uint64
	retries         *settlementRetries

	// chain reads blocks and logs; it is ethClient outside tests.
	chain chainReader
	// pendingReplay is a block range whose replay failed and is retried by the
	// listener; initialReplayDone records that the 24-hour startup replay was
	// attempted in this process. Both are used only by the listener goroutine.
	pendingReplay     *blockRange
	initialReplayDone bool
	// resubscribeFailures counts subscriptions in a row that failed before
	// lasting stableSubscription; it sets the wait before the next resubscribe.
	// Used only by the listener goroutine.
	resubscribeFailures int
	// wait pauses for d, or until ctx ends; now tells the time. Tests replace both.
	wait func(ctx context.Context, d time.Duration) error
	now  func() time.Time
}

// Product represents a purchasable subscription product.
type Product struct {
	ID              string  `json:"id"`
	Name            string  `json:"name"`
	Description     string  `json:"description"`
	PriceUSD        float64 `json:"price_usd"`
	DiscountPercent float64 `json:"discount_percent"`
	DiscountedPrice float64 `json:"discounted_price"`
	Tier            string  `json:"tier"`
	IsLifetime      bool    `json:"is_lifetime"`
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

// Blocks on Base come about every 2 seconds.
const (
	startupSyncBlocks     = 43_200 // ~24 hours, the lifetime of a payment intent
	resubscribeSyncBlocks = 1_800  // ~1 hour
	// replayBatchBlocks is the most blocks one log query covers. Free RPC plans
	// refuse wider ranges (dRPC's at more than 100 blocks), and dRPC then closes
	// the WebSocket, which ends the subscription too.
	replayBatchBlocks = 100
	// replayBatchPause spaces a replay's log queries, so a replay stays under
	// public endpoints' rate limits: the 24-hour startup replay takes about two
	// minutes.
	replayBatchPause = 250 * time.Millisecond
	// A subscription that ends sooner than stableSubscription after it started
	// counts as failed, and the listener waits longer before each further
	// resubscribe, up to maxResubscribeWait, instead of reconnecting in a loop.
	stableSubscription           = time.Minute
	maxResubscribeWait           = time.Minute
	settlementRetryCheckInterval = time.Minute
	// Live events arriving while a replay runs wait here (the client buffers more).
	liveLogBuffer = 1024

	// paymentIntentLifetime matches the expiry worker's query and the claim
	// query: a payment is accepted only within this long of the intent's creation.
	paymentIntentLifetime = 24 * time.Hour
)

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

func NewService(db *sql.DB, queries pgdb.Querier, logger *logger.Logger) *Service {
	return &Service{
		db:        db,
		queries:   queries,
		logger:    logger,
		coingecko: NewCoinGeckoService(logger),
		retries:   newSettlementRetries(),
		wait:      sleepContext,
		now:       time.Now,
	}
}

// resubscribeWait is how long the listener waits before resubscribing after
// failures subscriptions in a row ended early: nothing after the first, then
// 1 s, doubling up to maxResubscribeWait.
func resubscribeWait(failures int) time.Duration {
	if failures <= 0 {
		return 0
	}
	if failures > 7 {
		return maxResubscribeWait
	}
	return min(time.Second<<(failures-1), maxResubscribeWait)
}

// sleepContext waits for d, or until ctx ends.
func sleepContext(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
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
	s.chain = ethClient
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
	discount := config.AppConfig.FaiDiscountPercent

	products := []Product{
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

	for i := range products {
		products[i].DiscountPercent = discount
		if discount > 0 {
			products[i].DiscountedPrice = products[i].PriceUSD * (1 - discount/100)
		} else {
			products[i].DiscountedPrice = products[i].PriceUSD
		}
	}

	return products
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

	// Use discounted price for FAI amount calculation
	chargePrice := product.DiscountedPrice

	id := uuid.New().String()
	err = s.queries.CreateFaiPaymentIntent(ctx, pgdb.CreateFaiPaymentIntentParams{
		ID:        id,
		UserID:    userID,
		PaymentID: paymentID,
		ProductID: productID,
		PriceUsd:  chargePrice,
		FaiPrice:  sql.NullFloat64{Float64: faiPrice, Valid: faiPrice > 0},
	})
	if err != nil {
		return nil, fmt.Errorf("failed to store payment intent: %w", err)
	}

	faiAmount, faiAmountWei := computeFaiAmountFields(chargePrice, faiPrice)

	s.logger.Info("created FAI payment intent",
		"payment_id", paymentID,
		"user_id", userID,
		"product_id", productID,
		"original_price_usd", product.PriceUSD,
		"discount_percent", product.DiscountPercent,
		"charge_price_usd", chargePrice,
		"fai_price", faiPrice,
		"fai_amount", faiAmount)

	return &PaymentIntentResponse{
		PaymentID:    paymentID,
		ProductID:    productID,
		PriceUSD:     chargePrice,
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
//
// It subscribes before it reads the chain head and replays up to it, so every
// event lands in the replay, the subscription, or both. Live events that arrive
// during the replay wait in the channel. Overlap is harmless: an intent is
// claimed once.
func (s *Service) StartEventListener(ctx context.Context) error {
	if s.chain == nil {
		return fmt.Errorf("blockchain not initialized - call InitBlockchain first")
	}

	s.logger.Info("starting FAI payment event listener",
		"contract_address", s.contractAddress.Hex(),
		"event_signature", s.eventSignature.Hex())

	logs := make(chan types.Log, liveLogBuffer)
	sub, err := s.subscribe(ctx, logs)
	if err != nil {
		return fmt.Errorf("failed to subscribe to logs: %w", err)
	}
	defer func() {
		if sub != nil {
			sub.Unsubscribe()
		}
	}()
	subscribedAt := s.now()
	s.logger.Info("subscribed to PaymentReceived events")

	// The first start in this process replays the last 24 hours: settlement is
	// idempotent, and this picks up any event whose settlement failed before a
	// restart. Later restarts of the listener replay an hour, like resubscribes.
	lookback := uint64(resubscribeSyncBlocks)
	if !s.initialReplayDone {
		lookback = startupSyncBlocks
	}
	if err := s.catchUp(ctx, lookback); err != nil {
		return err
	}
	s.initialReplayDone = true

	retryTicker := time.NewTicker(settlementRetryCheckInterval)
	defer retryTicker.Stop()

	for {
		select {
		case err := <-sub.Err():
			s.logger.Error("subscription error, resubscribing", "error", err)
			sub.Unsubscribe() // release the failed one; safe to repeat
			if s.now().Sub(subscribedAt) >= stableSubscription {
				s.resubscribeFailures = 0
			}
			if wait := resubscribeWait(s.resubscribeFailures); wait > 0 {
				s.logger.Info("waiting before resubscribing", "wait", wait.String())
				if err := s.wait(ctx, wait); err != nil {
					return err
				}
			}
			s.resubscribeFailures++
			next, err := s.subscribe(ctx, logs)
			if err != nil {
				return fmt.Errorf("failed to resubscribe: %w", err)
			}
			sub = next
			subscribedAt = s.now()
			s.logger.Info("resubscribed to PaymentReceived events")
			if err := s.catchUp(ctx, resubscribeSyncBlocks); err != nil {
				return err
			}

		case vLog := <-logs:
			s.logger.Info("received log", "block_number", vLog.BlockNumber, "tx_hash", vLog.TxHash.Hex())
			s.handleEvent(ctx, vLog)

		case <-retryTicker.C:
			s.retryPendingReplay(ctx)
			s.retryFailedSettlements(ctx, time.Now())

		case <-ctx.Done():
			s.logger.Info("stopping event listener")
			return ctx.Err()
		}
	}
}

// subscribe starts a live subscription to PaymentReceived events.
func (s *Service) subscribe(ctx context.Context, logs chan<- types.Log) (ethereum.Subscription, error) {
	return s.chain.SubscribeFilterLogs(ctx, ethereum.FilterQuery{
		Addresses: []common.Address{s.contractAddress},
		Topics:    [][]common.Hash{{s.eventSignature}},
	}, logs)
}

// catchUp replays the last lookback blocks up to the current head. Call it after
// subscribing. A failed replay is kept pending and retried by the listener.
func (s *Service) catchUp(ctx context.Context, lookback uint64) error {
	head, err := s.chain.BlockNumber(ctx)
	if err != nil {
		return fmt.Errorf("failed to get current block number: %w", err)
	}
	if err := s.replay(ctx, lookbackRange(head, lookback)); err != nil {
		s.logger.Warn("historical replay failed; retrying it while listening", "error", err.Error())
	}
	return nil
}

// blockTime returns when a block was mined.
func (s *Service) blockTime(ctx context.Context, blockNumber uint64) (time.Time, error) {
	if s.chain == nil {
		return time.Time{}, fmt.Errorf("blockchain not initialized")
	}
	header, err := s.chain.HeaderByNumber(ctx, new(big.Int).SetUint64(blockNumber))
	if err != nil {
		return time.Time{}, err
	}
	return time.Unix(int64(header.Time), 0), nil
}

// handleEvent processes one payment event and queues it for another attempt when
// it failed for a transient reason.
func (s *Service) handleEvent(ctx context.Context, vLog types.Log) {
	err := s.handlePaymentReceivedEvent(ctx, vLog)
	if err == nil {
		return
	}
	s.logger.Error("failed to handle payment event",
		"error", err.Error(),
		"block", vLog.BlockNumber,
		"tx_hash", vLog.TxHash.Hex(),
		"will_retry", isRetryable(err))
	if isRetryable(err) {
		s.retries.add(vLog, time.Now())
	}
}

// retryFailedSettlements tries again every queued event that is due.
func (s *Service) retryFailedSettlements(ctx context.Context, now time.Time) {
	for _, vLog := range s.retries.due(now) {
		err := s.handlePaymentReceivedEvent(ctx, vLog)
		if gaveUp := s.retries.result(vLog, err, now); gaveUp {
			s.logger.Error("giving up on settling a payment event; settle it by hand",
				"error", err.Error(), "block", vLog.BlockNumber, "tx_hash", vLog.TxHash.Hex())
			continue
		}
		if err != nil {
			s.logger.Warn("payment settlement retry failed",
				"error", err.Error(), "block", vLog.BlockNumber, "tx_hash", vLog.TxHash.Hex(),
				"will_retry", isRetryable(err))
		} else {
			s.logger.Info("payment settlement retry succeeded", "block", vLog.BlockNumber, "tx_hash", vLog.TxHash.Hex())
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
		return retryable(fmt.Errorf("failed to query payment intent: %w", err))
	}

	if intent.Status != "pending" && intent.Status != "expired" {
		s.logger.Info("payment intent already settled, skipping", "payment_id", paymentIDHex, "status", intent.Status)
		return nil
	}

	// A payment is credited when it was made while its intent was open, whatever
	// the intent's status is now: settlement can lag the payment (retries, listener
	// downtime) and the expiry worker may have retired the intent meanwhile. A
	// payment made after the deadline is not credited, since the price was locked
	// at creation. The claim below enforces the same rule in the database.
	paidAt, err := s.blockTime(ctx, vLog.BlockNumber)
	if err != nil {
		return retryable(fmt.Errorf("failed to read payment block time: %w", err))
	}
	deadline := intent.CreatedAt.Add(paymentIntentLifetime)
	if !paidAt.Before(deadline) {
		s.logger.Warn("payment made after its intent expired; not credited",
			"payment_id", paymentIDHex, "paid_at", paidAt, "deadline", deadline, "tx_hash", vLog.TxHash.Hex())
		return fmt.Errorf("payment %s made after its intent expired", paymentIDHex)
	}
	if intent.Status == "expired" {
		s.logger.Warn("settling an expired intent whose payment was made in time",
			"payment_id", paymentIDHex, "paid_at", paidAt, "deadline", deadline)
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
			return retryable(fmt.Errorf("failed to get token price: %w", err))
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

	txHash := vLog.TxHash.Hex()
	tokenAddr := tokenAddress.Hex()
	claimed, err := s.settlePayment(ctx, intent, product, pgdb.UpdateFaiPaymentIntentToCompletedParams{
		PaymentID:    paymentIDHex,
		TokenAddress: &tokenAddr,
		TokenAmount:  sql.NullFloat64{Float64: tokenAmountFloat, Valid: true},
		PaidBlock:    sql.NullInt64{Int64: int64(vLog.BlockNumber), Valid: true},
		TxHash:       &txHash,
		PaymentTime:  paidAt,
	})
	if err != nil {
		return retryable(err)
	}
	if !claimed {
		s.logger.Info("payment intent already settled, skipping", "payment_id", paymentIDHex)
		return nil
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

// settlePayment claims the pending intent and grants the entitlement in one
// transaction, and reports whether this call did it.
//
// The same PaymentReceived event reaches every replica's subscription and the
// historical sync. Granting before marking the intent completed, with nothing
// tying the two together, let each delivery grant another period. Claiming
// first serializes the deliveries on the row: the first one's UPDATE matches
// the pending row, the others wait for it to commit and then match nothing.
// If the grant fails, the rollback releases the claim for a later delivery.
func (s *Service) settlePayment(ctx context.Context, intent pgdb.FaiPaymentIntent, product *Product, claim pgdb.UpdateFaiPaymentIntentToCompletedParams) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	qtx := pgdb.New(tx)

	claimed, err := qtx.UpdateFaiPaymentIntentToCompleted(ctx, claim)
	if err != nil {
		return false, fmt.Errorf("failed to claim payment intent: %w", err)
	}
	if claimed == 0 {
		return false, nil
	}

	if err := s.grantEntitlement(ctx, qtx, intent, product); err != nil {
		return false, fmt.Errorf("failed to grant entitlement: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("failed to commit payment: %w", err)
	}
	return true, nil
}

// grantEntitlement grants the user a subscription based on the product purchased.
func (s *Service) grantEntitlement(ctx context.Context, q pgdb.Querier, intent pgdb.FaiPaymentIntent, product *Product) error {
	if product.IsLifetime {
		err := q.UpsertEntitlementWithTier(ctx, pgdb.UpsertEntitlementWithTierParams{
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

	err := q.UpsertEntitlementWithExtension(ctx, pgdb.UpsertEntitlementWithExtensionParams{
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

// lookbackRange is the range of the last lookback blocks up to current.
func lookbackRange(current, lookback uint64) blockRange {
	from := uint64(0)
	if current > lookback {
		from = current - lookback
	}
	return blockRange{from: from, to: current}
}

// replay handles the PaymentReceived events in r. If reading logs fails, the
// unread part of r is kept as pending and retried by the listener, so a failed
// replay is never mistaken for a completed one.
func (s *Service) replay(ctx context.Context, r blockRange) error {
	s.logger.Info("replaying payment events", "from_block", r.from, "to_block", r.to)

	for start := r.from; start <= r.to; start += replayBatchBlocks {
		if start > r.from {
			if err := s.wait(ctx, replayBatchPause); err != nil {
				s.addPendingReplay(blockRange{from: start, to: r.to})
				return err
			}
		}
		end := min(start+replayBatchBlocks-1, r.to)

		logs, err := s.chain.FilterLogs(ctx, ethereum.FilterQuery{
			FromBlock: new(big.Int).SetUint64(start),
			ToBlock:   new(big.Int).SetUint64(end),
			Addresses: []common.Address{s.contractAddress},
			Topics:    [][]common.Hash{{s.eventSignature}},
		})
		if err != nil {
			s.addPendingReplay(blockRange{from: start, to: r.to})
			return fmt.Errorf("failed to filter logs from block %d: %w", start, err)
		}

		for _, vLog := range logs {
			s.handleEvent(ctx, vLog)
		}
	}

	s.logger.Info("payment event replay completed", "to_block", r.to)
	return nil
}

// addPendingReplay records a range still to replay, merged with any already pending.
func (s *Service) addPendingReplay(r blockRange) {
	if p := s.pendingReplay; p != nil {
		r = blockRange{from: min(p.from, r.from), to: max(p.to, r.to)}
	}
	s.pendingReplay = &r
}

// retryPendingReplay replays the pending range, if any.
func (s *Service) retryPendingReplay(ctx context.Context) {
	if s.pendingReplay == nil {
		return
	}
	r := *s.pendingReplay
	s.pendingReplay = nil
	if err := s.replay(ctx, r); err != nil {
		s.logger.Warn("historical replay retry failed", "error", err.Error())
	}
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
