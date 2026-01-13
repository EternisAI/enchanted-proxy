package memory

import (
	"context"
	"fmt"
	"strings"

	"github.com/eternisai/enchanted-proxy/internal/logger"
	pgdb "github.com/eternisai/enchanted-proxy/internal/storage/pg/sqlc"
)

// Service handles user memory/facts retrieval and formatting.
type Service struct {
	queries *pgdb.Queries
	logger  *logger.Logger
}

// NewService creates a new memory service.
func NewService(queries *pgdb.Queries, log *logger.Logger) *Service {
	return &Service{
		queries: queries,
		logger:  log.WithComponent("memory"),
	}
}

// GetFormattedMemory fetches all facts for a user and formats them into a prompt string.
// Returns empty string if user has no facts.
func (s *Service) GetFormattedMemory(ctx context.Context, userID string) (string, error) {
	if userID == "" {
		return "", nil
	}

	facts, err := s.queries.GetUserFactsByUserID(ctx, userID)
	if err != nil {
		s.logger.Error("failed to fetch user facts",
			"error", err.Error(),
			"user_id", userID,
		)
		return "", fmt.Errorf("failed to fetch user facts: %w", err)
	}

	if len(facts) == 0 {
		return "", nil
	}

	// Group facts by type
	workContext := []string{}
	personalContext := []string{}
	topOfMind := []string{}

	for _, fact := range facts {
		switch fact.FactType {
		case "work_context":
			workContext = append(workContext, fmt.Sprintf("- %s", fact.FactBody))
		case "personal_context":
			personalContext = append(personalContext, fmt.Sprintf("- %s", fact.FactBody))
		case "top_of_mind":
			topOfMind = append(topOfMind, fmt.Sprintf("- %s", fact.FactBody))
		}
	}

	// Build formatted memory string
	var sections []string

	if len(workContext) > 0 || len(personalContext) > 0 || len(topOfMind) > 0 {
		sections = append(sections, "Users facts and memories:")
	}

	if len(workContext) > 0 {
		sections = append(sections, "**Work context**")
		sections = append(sections, strings.Join(workContext, "\n"))
	}

	if len(personalContext) > 0 {
		sections = append(sections, "**Personal context**")
		sections = append(sections, strings.Join(personalContext, "\n"))
	}

	if len(topOfMind) > 0 {
		sections = append(sections, "**Top of mind**")
		sections = append(sections, strings.Join(topOfMind, "\n"))
	}

	if len(sections) == 0 {
		return "", nil
	}

	formattedMemory := strings.Join(sections, "\n\n")

	s.logger.Info("formatted user memory",
		"user_id", userID,
		"work_context_count", len(workContext),
		"personal_context_count", len(personalContext),
		"top_of_mind_count", len(topOfMind),
	)

	return formattedMemory, nil
}
