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

func (s *Service) LogRequest(info RequestInfo) error {
	ctx := context.Background()

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

func (s *Service) CheckRateLimit(userID string, maxRequestsPerDay int64) (bool, error) {
	ctx := context.Background()

	count, err := s.queries.GetUserRequestCountInLastDay(ctx, userID)
	if err != nil {
		return false, fmt.Errorf("failed to check rate limit: %w", err)
	}

	return count < maxRequestsPerDay, nil
}

func (s *Service) GetUserRequestCountSince(userID string, since time.Time) (int64, error) {
	ctx := context.Background()
	params := pgdb.GetUserRequestCountInTimeWindowParams{
		UserID:    userID,
		CreatedAt: since,
	}
	return s.queries.GetUserRequestCountInTimeWindow(ctx, params)
}

func (s *Service) RefreshMaterializedView() error {
	ctx := context.Background()
	return s.queries.RefreshUserRequestCountsView(ctx)
}

// GetProviderFromBaseURL maps base URLs to provider names
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
