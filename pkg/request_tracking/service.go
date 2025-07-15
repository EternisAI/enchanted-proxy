package request_tracking

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/log"
	"github.com/eternisai/enchanted-proxy/pkg/config"
	pgdb "github.com/eternisai/enchanted-proxy/pkg/storage/pg/sqlc"
)

type Service struct {
	queries    pgdb.Querier
	logChan    chan logRequest
	workerPool sync.WaitGroup
	shutdown   chan struct{}
	logger     *log.Logger
}

type logRequest struct {
	ctx  context.Context
	info RequestInfo
}

func NewService(queries pgdb.Querier, logger *log.Logger) *Service {
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
			s.processLogRequest(logReq.ctx, logReq.info)
		case <-s.shutdown:
			// Process remaining log requests before shutdown.
			for {
				select {
				case logReq := <-s.logChan:
					s.processLogRequest(logReq.ctx, logReq.info)
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

	params := pgdb.CreateRequestLogParams{
		UserID:   info.UserID,
		Endpoint: info.Endpoint,
		Model:    model,
		Provider: info.Provider,
	}

	if err := s.queries.CreateRequestLog(ctx, params); err != nil {
		s.logger.Error("Failed to insert request log",
			"user_id", info.UserID,
			"endpoint", info.Endpoint,
			"provider", info.Provider,
			"error", err)
	}
}

// LogRequestAsync queues a log request to be processed by the worker pool.
func (s *Service) LogRequestAsync(ctx context.Context, info RequestInfo) error {
	logCtx := ctx

	// Use the caller's context if possible, otherwise create a reasonable timeout.
	if deadline, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		logCtx, cancel = context.WithTimeout(context.Background(), time.Duration(config.AppConfig.RequestTrackingTimeoutSeconds)*time.Second)
		_ = cancel
	} else {
		remaining := time.Until(deadline)
		if remaining < time.Second {
			var cancel context.CancelFunc
			logCtx, cancel = context.WithTimeout(context.Background(), time.Duration(config.AppConfig.RequestTrackingTimeoutSeconds)*time.Second)
			_ = cancel
		}
	}

	logReq := logRequest{
		ctx:  logCtx,
		info: info,
	}

	select {
	case s.logChan <- logReq:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	default:
		s.logger.Warn("Request log queue is full, dropping request",
			"user_id", info.UserID,
			"endpoint", info.Endpoint)
		return fmt.Errorf("log queue is full, dropping request")
	}
}

// Shutdown gracefully shuts down the worker pool
func (s *Service) Shutdown() {
	close(s.shutdown)
	s.workerPool.Wait()
	close(s.logChan)
}

type RequestInfo struct {
	UserID   string
	Endpoint string
	Model    string
	Provider string
}

func (s *Service) CheckRateLimit(ctx context.Context, userID string, maxRequestsPerDay int64) (bool, error) {
	count, err := s.queries.GetUserRequestCountInLastDay(ctx, userID)
	if err != nil {
		return false, fmt.Errorf("failed to check rate limit: %w", err)
	}

	return count < maxRequestsPerDay, nil
}

func (s *Service) GetUserRequestCountSince(ctx context.Context, userID string, since time.Time) (int64, error) {
	params := pgdb.GetUserRequestCountInTimeWindowParams{
		UserID:    userID,
		CreatedAt: since,
	}
	return s.queries.GetUserRequestCountInTimeWindow(ctx, params)
}

func (s *Service) RefreshMaterializedView(ctx context.Context) error {
	return s.queries.RefreshUserRequestCountsView(ctx)
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
