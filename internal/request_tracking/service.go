package request_tracking

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/eternisai/enchanted-proxy/internal/config"
	"github.com/eternisai/enchanted-proxy/internal/logger"
	pgdb "github.com/eternisai/enchanted-proxy/internal/storage/pg/sqlc"
	"github.com/eternisai/enchanted-proxy/internal/tiers"
)

type Service struct {
	queries              pgdb.Querier
	logChan              chan logRequest
	workerPool           sync.WaitGroup
	shutdown             chan struct{}
	closed               atomic.Bool
	logger               *logger.Logger
	droppedRequestsTotal atomic.Int64 // Track dropped requests due to queue overflow.

	// workerCtx is the parent context for every DB write. Cancelled by
	// Shutdown when the bounded drain deadline is exceeded, which forces
	// in-flight pgx calls to abort instead of holding shutdown open.
	workerCtx    context.Context
	workerCancel context.CancelFunc
}

type logRequest struct {
	info RequestInfo
}

func NewService(queries pgdb.Querier, logger *logger.Logger) *Service {
	workerCtx, workerCancel := context.WithCancel(context.Background())
	s := &Service{
		queries:      queries,
		logChan:      make(chan logRequest, config.AppConfig.RequestTrackingBufferSize),
		shutdown:     make(chan struct{}),
		logger:       logger,
		workerCtx:    workerCtx,
		workerCancel: workerCancel,
	}

	// Worker pool with configurable number of workers.
	for i := 0; i < config.AppConfig.RequestTrackingWorkerPoolSize; i++ {
		s.workerPool.Add(1)
		go s.logWorker()
	}

	return s
}

// logWorker processes log requests from the channel.
func (s *Service) logWorker() {
	defer s.workerPool.Done()

	for {
		select {
		case logReq := <-s.logChan:
			s.handleLogRequest(logReq)
		case <-s.shutdown:
			// Process remaining log requests before shutdown.
			for {
				select {
				case logReq := <-s.logChan:
					s.handleLogRequest(logReq)
				default:
					return
				}
			}
		}
	}
}

// processLogRequest handles the actual database insertion.
func (s *Service) processLogRequest(ctx context.Context, info RequestInfo) {
	var model *string
	if info.Model != "" {
		model = &info.Model
	}

	var promptTokens, completionTokens, totalTokens sql.NullInt32
	if info.PromptTokens != nil {
		promptTokens = sql.NullInt32{Int32: int32(*info.PromptTokens), Valid: true}
	}
	if info.CompletionTokens != nil {
		completionTokens = sql.NullInt32{Int32: int32(*info.CompletionTokens), Valid: true}
	}
	if info.TotalTokens != nil {
		totalTokens = sql.NullInt32{Int32: int32(*info.TotalTokens), Valid: true}
	}

	// Use new query with plan tokens if available, otherwise use old query
	if info.PlanTokens != nil && info.Multiplier != nil {
		params := pgdb.CreateRequestLogWithPlanTokensParams{
			UserID:           info.UserID,
			Endpoint:         info.Endpoint,
			Model:            model,
			Provider:         info.Provider,
			PromptTokens:     promptTokens,
			CompletionTokens: completionTokens,
			TotalTokens:      totalTokens,
			PlanTokens:       sql.NullInt32{Int32: int32(*info.PlanTokens), Valid: true},
			// Note: TokenMultiplier uses string formatting because sqlc generates sql.NullString
			// for NUMERIC(8,2) columns. PostgreSQL converts strings to NUMERIC on insert.
			// This is standard sqlc behavior for NUMERIC types.
			TokenMultiplier: sql.NullString{String: fmt.Sprintf("%.2f", *info.Multiplier), Valid: true},
		}

		if err := s.queries.CreateRequestLogWithPlanTokens(ctx, params); err != nil {
			s.logger.Error("failed to insert request log with plan tokens",
				slog.String("user_id", info.UserID),
				slog.String("endpoint", info.Endpoint),
				slog.String("model", info.Model),
				slog.String("provider", info.Provider),
				slog.Int("prompt_tokens", intValue(info.PromptTokens)),
				slog.Int("completion_tokens", intValue(info.CompletionTokens)),
				slog.Int("total_tokens", intValue(info.TotalTokens)),
				slog.Int("plan_tokens", intValue(info.PlanTokens)),
				slog.Float64("multiplier", float64Value(info.Multiplier)),
				slog.String("error", err.Error()))
			return
		}

		s.logger.Debug("inserted request log with plan tokens",
			slog.String("user_id", info.UserID),
			slog.String("endpoint", info.Endpoint),
			slog.String("model", info.Model),
			slog.String("provider", info.Provider),
			slog.Int("total_tokens", intValue(info.TotalTokens)),
			slog.Int("plan_tokens", intValue(info.PlanTokens)),
			slog.Float64("multiplier", float64Value(info.Multiplier)))
	} else {
		// Fallback to old query for backward compatibility
		params := pgdb.CreateRequestLogParams{
			UserID:           info.UserID,
			Endpoint:         info.Endpoint,
			Model:            model,
			Provider:         info.Provider,
			PromptTokens:     promptTokens,
			CompletionTokens: completionTokens,
			TotalTokens:      totalTokens,
		}

		if err := s.queries.CreateRequestLog(ctx, params); err != nil {
			s.logger.Error("failed to insert request log",
				slog.String("user_id", info.UserID),
				slog.String("endpoint", info.Endpoint),
				slog.String("model", info.Model),
				slog.String("provider", info.Provider),
				slog.Int("prompt_tokens", intValue(info.PromptTokens)),
				slog.Int("completion_tokens", intValue(info.CompletionTokens)),
				slog.Int("total_tokens", intValue(info.TotalTokens)),
				slog.String("error", err.Error()))
			return
		}

		s.logger.Debug("inserted request log",
			slog.String("user_id", info.UserID),
			slog.String("endpoint", info.Endpoint),
			slog.String("model", info.Model),
			slog.String("provider", info.Provider),
			slog.Int("total_tokens", intValue(info.TotalTokens)))
	}
}

func intValue(value *int) int {
	if value == nil {
		return 0
	}
	return *value
}

func float64Value(value *float64) float64 {
	if value == nil {
		return 0
	}
	return *value
}

// LogRequestAsync queues a log request to be processed by the worker pool.
func (s *Service) LogRequestAsync(ctx context.Context, info RequestInfo) error {
	if s.closed.Load() {
		s.logger.Warn("Request tracking service is shutting down, dropping request",
			slog.String("user_id", info.UserID),
			slog.String("endpoint", info.Endpoint))
		return fmt.Errorf("service shutting down")
	}

	// The caller's context governs ONLY the queue-insertion attempt. Once the
	// request is queued, it must complete on its own clock — the caller's
	// context is often cancelled long before a worker dequeues (e.g. the
	// background polling worker uses a short-lived `context.WithTimeout` plus
	// `defer cancel` purely to bound this call). Storing the caller's context
	// on the queued request caused every GPT-5 Pro DB write to fail with
	// `context canceled`, silently dropping the row and letting users bypass
	// per-tier plan-token quotas. The worker creates its own fresh context.
	logReq := logRequest{
		info: info,
	}

	select {
	case s.logChan <- logReq:
		s.logger.Debug("queued request log",
			slog.String("user_id", info.UserID),
			slog.String("endpoint", info.Endpoint),
			slog.String("model", info.Model),
			slog.String("provider", info.Provider),
			slog.Int("total_tokens", intValue(info.TotalTokens)),
			slog.Int("plan_tokens", intValue(info.PlanTokens)),
			slog.Float64("multiplier", float64Value(info.Multiplier)),
			slog.Int("queue_depth", len(s.logChan)),
			slog.Int("queue_capacity", cap(s.logChan)))
		return nil
	case <-ctx.Done():
		s.logger.Warn("request log enqueue canceled",
			slog.String("user_id", info.UserID),
			slog.String("endpoint", info.Endpoint),
			slog.String("model", info.Model),
			slog.String("provider", info.Provider),
			slog.Int("total_tokens", intValue(info.TotalTokens)),
			slog.Int("plan_tokens", intValue(info.PlanTokens)),
			slog.String("error", ctx.Err().Error()))
		return ctx.Err()
	default:
		// Queue is full - increment counter and log error
		dropped := s.droppedRequestsTotal.Add(1)
		s.logger.Error("Request log queue FULL - request DROPPED",
			slog.String("user_id", info.UserID),
			slog.String("endpoint", info.Endpoint),
			slog.String("model", info.Model),
			slog.String("provider", info.Provider),
			slog.Int("total_tokens", intValue(info.TotalTokens)),
			slog.Int("plan_tokens", intValue(info.PlanTokens)),
			slog.Float64("multiplier", float64Value(info.Multiplier)),
			slog.Int64("total_dropped", dropped),
			slog.Int("queue_depth", len(s.logChan)),
			slog.Int("queue_capacity", cap(s.logChan)))
		return fmt.Errorf("log queue is full, dropping request")
	}
}

// Shutdown gracefully shuts down the worker pool.
//
// Workers drain the queue under their normal per-write timeout. If the
// supplied context expires before the drain completes, in-flight DB writes
// are cancelled via workerCancel so this method always returns. Bounding
// shutdown latency matters because each pgx call can otherwise consume the
// full RequestTrackingTimeoutSeconds, and `worker_count × queue_depth × timeout`
// can run into hours under DB trouble.
//
// logChan is intentionally NOT closed: LogRequestAsync checks `closed` and
// then sends — closing the channel would create a send-on-closed-channel
// panic race against any caller that has already passed the closed check.
// The channel is unreferenced after Shutdown returns and is collected.
func (s *Service) Shutdown(ctx context.Context) error {
	s.closed.Store(true)
	close(s.shutdown)

	done := make(chan struct{})
	go func() {
		s.workerPool.Wait()
		close(done)
	}()

	select {
	case <-done:
		s.workerCancel()
		return nil
	case <-ctx.Done():
		// Force in-flight DB writes to abort.
		s.workerCancel()
		<-done
		return ctx.Err()
	}
}

// handleLogRequest builds a fresh timeout context for the DB write derived
// from workerCtx. The caller's context is deliberately not propagated — see
// LogRequestAsync.
func (s *Service) handleLogRequest(lr logRequest) {
	ctx, cancel := context.WithTimeout(
		s.workerCtx,
		time.Duration(config.AppConfig.RequestTrackingTimeoutSeconds)*time.Second,
	)
	defer cancel()

	s.processLogRequest(ctx, lr.info)
}

type RequestInfo struct {
	UserID           string
	Endpoint         string
	Model            string
	Provider         string
	PromptTokens     *int
	CompletionTokens *int
	TotalTokens      *int     // Raw tokens from API (existing field)
	PlanTokens       *int     // NEW: Weighted tokens (TotalTokens × Multiplier)
	Multiplier       *float64 // NEW: Cost multiplier
}

// HasActivePro checks if user has an active Pro entitlement and returns expiry when available.
func (s *Service) HasActivePro(ctx context.Context, userID string) (bool, *time.Time, error) {
	ent, err := s.queries.GetEntitlement(ctx, userID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil, nil
		}
		return false, nil, err
	}
	now := time.Now().UTC()
	if ent.SubscriptionExpiresAt.Valid && ent.SubscriptionExpiresAt.Time.After(now) {
		t := ent.SubscriptionExpiresAt.Time
		return true, &t, nil
	}
	return false, nil, nil
}

// GetSubscriptionProvider returns the subscription provider for a user (e.g., "apple", "stripe").
// Returns empty string if user has no entitlement record.
func (s *Service) GetSubscriptionProvider(ctx context.Context, userID string) (string, error) {
	ent, err := s.queries.GetEntitlement(ctx, userID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil
		}
		return "", err
	}
	if ent.SubscriptionProvider != "" {
		return ent.SubscriptionProvider, nil
	}
	return "", nil
}

// LogRequestWithTokensAsync queues a log request with token data to be processed by the worker pool.
func (s *Service) LogRequestWithTokensAsync(ctx context.Context, info RequestInfo, tokenData *TokenUsage) error {
	if tokenData != nil {
		info.PromptTokens = &tokenData.PromptTokens
		info.CompletionTokens = &tokenData.CompletionTokens
		info.TotalTokens = &tokenData.TotalTokens
	}

	return s.LogRequestAsync(ctx, info)
}

// TokenUsage represents token usage data from API responses.
type TokenUsage struct {
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
}

// TokenUsageWithMultiplier represents token usage with cost weighting.
type TokenUsageWithMultiplier struct {
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int     // Raw model tokens
	Multiplier       float64 // Cost multiplier
	PlanTokens       int     // TotalTokens × Multiplier
}

// GetUserTier returns the user's current subscription tier.
func (s *Service) GetUserTier(ctx context.Context, userID string) (tiers.Tier, *time.Time, error) {
	result, err := s.queries.GetUserTier(ctx, userID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// User has no entitlement record, default to free
			return tiers.TierFree, nil, nil
		}
		return "", nil, fmt.Errorf("failed to get user tier: %w", err)
	}

	tier := tiers.Tier(result.SubscriptionTier)

	// Check if tier has expired
	var expiresAt *time.Time
	if result.SubscriptionExpiresAt.Valid {
		expiresAt = &result.SubscriptionExpiresAt.Time
		if expiresAt.Before(time.Now().UTC()) {
			// Tier expired, downgrade to free
			s.logger.Info("user tier expired, downgrading to free",
				slog.String("user_id", userID),
				slog.String("expired_tier", string(tier)),
				slog.Time("expired_at", *expiresAt))
			return tiers.TierFree, nil, nil
		}
	}

	return tier, expiresAt, nil
}

// GetUserTierConfig returns the full tier configuration for a user.
func (s *Service) GetUserTierConfig(ctx context.Context, userID string) (tiers.Config, *time.Time, error) {
	tier, expiresAt, err := s.GetUserTier(ctx, userID)
	if err != nil {
		return tiers.Config{}, nil, err
	}

	config, err := tiers.Get(tier)
	if err != nil {
		// Fallback to free if tier not found
		config = tiers.Configs[tiers.TierFree]
	}

	return config, expiresAt, nil
}

// LogRequestWithPlanTokensAsync queues a request with plan token calculation.
func (s *Service) LogRequestWithPlanTokensAsync(
	ctx context.Context,
	info RequestInfo,
	tokenData *TokenUsageWithMultiplier,
) error {
	if tokenData != nil {
		info.PromptTokens = &tokenData.PromptTokens
		info.CompletionTokens = &tokenData.CompletionTokens
		info.TotalTokens = &tokenData.TotalTokens
		info.PlanTokens = &tokenData.PlanTokens
		info.Multiplier = &tokenData.Multiplier
	}

	return s.LogRequestAsync(ctx, info)
}

// GetUserPlanTokensThisWeek returns plan tokens used this week.
func (s *Service) GetUserPlanTokensThisWeek(ctx context.Context, userID string) (int64, error) {
	result, err := s.queries.GetUserPlanTokensThisWeek(ctx, userID)
	if err != nil {
		return 0, fmt.Errorf("failed to get weekly plan tokens: %w", err)
	}
	return result, nil
}

// GetUserPlanTokensThisMonth returns plan tokens used this month.
func (s *Service) GetUserPlanTokensThisMonth(ctx context.Context, userID string) (int64, error) {
	result, err := s.queries.GetUserPlanTokensThisMonth(ctx, userID)
	if err != nil {
		return 0, fmt.Errorf("failed to get monthly plan tokens: %w", err)
	}
	return result, nil
}

// GetUserPlanTokensToday returns plan tokens used today.
func (s *Service) GetUserPlanTokensToday(ctx context.Context, userID string) (int64, error) {
	result, err := s.queries.GetUserPlanTokensToday(ctx, userID)
	if err != nil {
		return 0, fmt.Errorf("failed to get daily plan tokens: %w", err)
	}
	return result, nil
}

// GetUserFallbackPlanTokensToday returns plan tokens used today on the fallback model.
func (s *Service) GetUserFallbackPlanTokensToday(ctx context.Context, userID string, fallbackModel string) (int64, error) {
	if fallbackModel == "" {
		return 0, nil
	}
	result, err := s.queries.GetUserFallbackPlanTokensToday(ctx, pgdb.GetUserFallbackPlanTokensTodayParams{
		UserID: userID,
		Model:  &fallbackModel,
	})
	if err != nil {
		return 0, fmt.Errorf("failed to get daily fallback plan tokens: %w", err)
	}
	return result, nil
}

// GetUserDeepResearchRunsToday returns deep research runs today.
func (s *Service) GetUserDeepResearchRunsToday(ctx context.Context, userID string) (int64, error) {
	result, err := s.queries.GetUserDeepResearchRunsToday(ctx, userID)
	if err != nil {
		return 0, fmt.Errorf("failed to get daily deep research runs: %w", err)
	}
	return result, nil
}

// GetUserDeepResearchRunsLifetime returns deep research runs lifetime.
func (s *Service) GetUserDeepResearchRunsLifetime(ctx context.Context, userID string) (int64, error) {
	result, err := s.queries.GetUserDeepResearchRunsLifetime(ctx, userID)
	if err != nil {
		return 0, fmt.Errorf("failed to get lifetime deep research runs: %w", err)
	}
	return result, nil
}

// GetMetrics returns diagnostic metrics for request tracking.
func (s *Service) GetMetrics() map[string]int64 {
	return map[string]int64{
		"dropped_requests_total": s.droppedRequestsTotal.Load(),
		"queue_size":             int64(len(s.logChan)),
		"queue_capacity":         int64(config.AppConfig.RequestTrackingBufferSize),
	}
}
