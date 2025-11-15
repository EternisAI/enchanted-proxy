package request_tracking

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/eternisai/enchanted-proxy/internal/config"
	"github.com/eternisai/enchanted-proxy/internal/logger"
	pgdb "github.com/eternisai/enchanted-proxy/internal/storage/pg/sqlc"
)

type Service struct {
	queries    pgdb.Querier
	logChan    chan logRequest
	workerPool sync.WaitGroup
	shutdown   chan struct{}
	closed     atomic.Bool
	logger     *logger.Logger
}

type logRequest struct {
	ctx  context.Context
	info RequestInfo
}

func NewService(queries pgdb.Querier, logger *logger.Logger) *Service {
	s := &Service{
		queries:  queries,
		logChan:  make(chan logRequest, config.AppConfig.RequestTrackingBufferSize),
		shutdown: make(chan struct{}),
		logger:   logger,
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
			slog.String("provider", info.Provider),
			slog.String("error", err.Error()))
	}
}

// LogRequestAsync queues a log request to be processed by the worker pool.
func (s *Service) LogRequestAsync(ctx context.Context, info RequestInfo) error {
	if s.closed.Load() {
		s.logger.Warn("Request tracking service is shutting down, dropping request",
			slog.String("user_id", info.UserID),
			slog.String("endpoint", info.Endpoint))
		return fmt.Errorf("service shutting down")
	}

	logReq := logRequest{
		ctx:  ctx,
		info: info,
	}

	select {
	case s.logChan <- logReq:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	default:
		s.logger.Warn("Request log queue is full, dropping request",
			slog.String("user_id", info.UserID),
			slog.String("endpoint", info.Endpoint))
		return fmt.Errorf("log queue is full, dropping request")
	}
}

// Shutdown gracefully shuts down the worker pool.
func (s *Service) Shutdown() {
	s.closed.Store(true)

	close(s.shutdown)
	s.workerPool.Wait()
	close(s.logChan)
}

// handleLogRequest ensures each request has a reasonable timeout and then processes it.
func (s *Service) handleLogRequest(lr logRequest) {
	ctx := lr.ctx

	var cancel context.CancelFunc
	if dl, ok := ctx.Deadline(); !ok || time.Until(dl) < time.Second {
		ctx, cancel = context.WithTimeout(context.Background(), time.Duration(config.AppConfig.RequestTrackingTimeoutSeconds)*time.Second)
	}

	s.processLogRequest(ctx, lr.info)

	if cancel != nil {
		cancel()
	}
}

type RequestInfo struct {
	UserID           string
	Endpoint         string
	Model            string
	Provider         string
	PromptTokens     *int
	CompletionTokens *int
	TotalTokens      *int
}

func (s *Service) CheckRateLimit(ctx context.Context, userID string, maxTokensPerDay int64) (bool, error) {
	count, err := s.queries.GetUserTokenUsageInLastDay(ctx, userID)
	if err != nil {
		return false, fmt.Errorf("failed to check rate limit: %w", err)
	}

	return count < maxTokensPerDay, nil
}

// GetUserLifetimeTokenUsage returns the total tokens a user has ever consumed.
func (s *Service) GetUserLifetimeTokenUsage(ctx context.Context, userID string) (int64, error) {
	return s.queries.GetUserLifetimeTokenUsage(ctx, userID)
}

// GetUserTokenUsageToday returns tokens used today.
func (s *Service) GetUserTokenUsageToday(ctx context.Context, userID string) (int64, error) {
	return s.queries.GetUserTokenUsageToday(ctx, userID)
}

// GetUserRequestCountToday returns requests made today.
func (s *Service) GetUserRequestCountToday(ctx context.Context, userID string) (int64, error) {
	return s.queries.GetUserRequestCountToday(ctx, userID)
}

func (s *Service) GetUserRequestCountSince(ctx context.Context, userID string, since time.Time) (int64, error) {
	sinceUTC := since.UTC()
	sinceDay := time.Date(
		sinceUTC.Year(), sinceUTC.Month(), sinceUTC.Day(),
		0, 0, 0, 0, time.UTC,
	)

	params := pgdb.GetUserRequestCountInTimeWindowParams{
		UserID:    userID,
		DayBucket: sinceDay,
	}
	return s.queries.GetUserRequestCountInTimeWindow(ctx, params)
}

func (s *Service) GetUserTokenUsageSince(ctx context.Context, userID string, since time.Time) (int64, error) {
	sinceUTC := since.UTC()
	sinceDay := time.Date(
		sinceUTC.Year(), sinceUTC.Month(), sinceUTC.Day(),
		0, 0, 0, 0, time.UTC,
	)

	params := pgdb.GetUserTokenUsageInTimeWindowParams{
		UserID:    userID,
		DayBucket: sinceDay,
	}
	return s.queries.GetUserTokenUsageInTimeWindow(ctx, params)
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
	if ent.ProExpiresAt.Valid && ent.ProExpiresAt.Time.After(now) {
		t := ent.ProExpiresAt.Time
		return true, &t, nil
	}
	return false, nil, nil
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

// GetProviderFromBaseURL maps base URLs to provider names.
func GetProviderFromBaseURL(baseURL string) string {
	baseURL = strings.TrimRight(baseURL, "/")

	switch baseURL {
	case "https://openrouter.ai/api/v1":
		return "openrouter"
	case "https://api.openai.com/v1":
		return "openai"
	case "https://audio-processing.model.tinfoil.sh/v1":
		return "tinfoil"
	default:
		return "unknown"
	}
}
