package request_tracking

import (
	"context"
	"fmt"
	"time"

	pgdb "github.com/eternisai/enchanted-proxy/pkg/storage/pg/sqlc"
)

type Service struct {
	queries pgdb.Querier
}

func NewService(queries pgdb.Querier) *Service {
	return &Service{queries: queries}
}

type RequestInfo struct {
	UserID   string
	Endpoint string
	Model    string
	Provider string
}

func (s *Service) LogRequest(ctx context.Context, info RequestInfo) error {
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

	return s.queries.CreateRequestLog(ctx, params)
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
	switch baseURL {
	case "https://openrouter.ai/api/v1":
		return "openrouter"
	case "https://api.openai.com/v1":
		return "openai"
	case "https://audio-processing.model.tinfoil.sh/v1/":
		return "tinfoil"
	default:
		return "unknown"
	}
}
