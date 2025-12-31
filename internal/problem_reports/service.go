package problem_reports

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"

	"github.com/eternisai/enchanted-proxy/internal/logger"
	pgdb "github.com/eternisai/enchanted-proxy/internal/storage/pg/sqlc"
	"github.com/google/uuid"
	"github.com/pgvector/pgvector-go"
)

type Service struct {
	queries         *pgdb.Queries
	embeddingClient *EmbeddingClient
	linearClient    *LinearClient
	logger          *logger.Logger
}

func NewService(queries *pgdb.Queries, openRouterAPIKey, linearAPIKey, linearTeamID, linearProjectID, linearLabelID string, logger *logger.Logger) *Service {
	return &Service{
		queries:         queries,
		embeddingClient: NewEmbeddingClient(openRouterAPIKey),
		linearClient:    NewLinearClient(linearAPIKey, linearTeamID, linearProjectID, linearLabelID),
		logger:          logger,
	}
}

func (s *Service) CreateReport(ctx context.Context, userID string, req *CreateProblemReportRequest) (*CreateProblemReportResponse, error) {
	log := s.logger.WithContext(ctx).WithComponent("problem-reports-service")

	count, err := s.queries.CountProblemReportsByUserID(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("failed to count user reports: %w", err)
	}

	if count >= MaxReportsPerUser {
		return nil, fmt.Errorf("user has reached maximum number of problem reports (%d)", MaxReportsPerUser)
	}

	var embeddingVec *pgvector.Vector
	embedding, err := s.embeddingClient.GetEmbedding(ctx, req.ProblemDescription)
	if err != nil {
		log.Error("failed to get embedding, continuing without duplicate detection", slog.String("error", err.Error()))
	} else {
		vec := pgvector.NewVector(embedding)
		embeddingVec = &vec
	}

	var parentID *string
	var ticketID *string
	isNewIssue := true

	if embeddingVec != nil {
		similarReports, err := s.queries.FindSimilarProblemReports(ctx, embeddingVec)
		if err != nil {
			log.Error("failed to find similar reports", slog.String("error", err.Error()))
		}

		for _, similar := range similarReports {
			if similar.Similarity >= SimilarityThreshold {
				log.Info("found similar report",
					slog.String("similar_id", similar.ID),
					slog.Float64("similarity", similar.Similarity))

				parentID = &similar.ID
				ticketID = similar.TicketID
				isNewIssue = false

				if similar.TicketID != nil {
					comment := fmt.Sprintf("**New duplicate report from user**\n\n**Description:**\n%s\n\n**Device:** %s (%s %s)\n**App Version:** %s (%s)",
						req.ProblemDescription,
						req.DeviceInfo.DeviceModel,
						req.DeviceInfo.SystemName,
						req.DeviceInfo.SystemVersion,
						req.DeviceInfo.AppVersion,
						req.DeviceInfo.BuildNumber)
					if err := s.linearClient.AddComment(ctx, *similar.TicketID, comment); err != nil {
						log.Error("failed to add Linear comment", slog.String("error", err.Error()))
					}
				}
				break
			}
		}
	}

	reportID := uuid.New().String()

	if isNewIssue {
		title := fmt.Sprintf("[Problem Report] %s", truncateString(req.ProblemDescription, 80))
		description := fmt.Sprintf("**Description:**\n%s\n\n**Device:** %s (%s %s)\n**App Version:** %s (%s)\n**Subscription Tier:** %s\n**Contact Email:** %s\n\n**Report ID:** %s\n**User ID:** %s",
			req.ProblemDescription,
			req.DeviceInfo.DeviceModel,
			req.DeviceInfo.SystemName,
			req.DeviceInfo.SystemVersion,
			req.DeviceInfo.AppVersion,
			req.DeviceInfo.BuildNumber,
			ptrToString(req.SubscriptionTier, "unknown"),
			ptrToString(req.ContactEmail, "not provided"),
			reportID,
			userID)

		linearTicketID, err := s.linearClient.CreateIssue(ctx, title, description)
		if err != nil {
			log.Error("failed to create Linear issue", slog.String("error", err.Error()))
		} else {
			ticketID = &linearTicketID
			log.Info("created Linear issue", slog.String("ticket_id", linearTicketID))
		}
	}

	params := pgdb.CreateProblemReportParams{
		ID:                 reportID,
		UserID:             userID,
		ProblemDescription: req.ProblemDescription,
		DeviceModel:        strPtr(req.DeviceInfo.DeviceModel),
		DeviceName:         strPtr(req.DeviceInfo.DeviceName),
		SystemName:         strPtr(req.DeviceInfo.SystemName),
		SystemVersion:      strPtr(req.DeviceInfo.SystemVersion),
		AppVersion:         strPtr(req.DeviceInfo.AppVersion),
		BuildNumber:        strPtr(req.DeviceInfo.BuildNumber),
		Locale:             strPtr(req.DeviceInfo.Locale),
		Timezone:           strPtr(req.DeviceInfo.Timezone),
		TotalCapacityBytes: sql.NullInt64{
			Int64: req.StorageInfo.TotalCapacityBytes,
			Valid: true,
		},
		AvailableCapacityBytes: sql.NullInt64{
			Int64: req.StorageInfo.AvailableCapacityBytes,
			Valid: true,
		},
		UsedCapacityBytes: sql.NullInt64{
			Int64: req.StorageInfo.UsedCapacityBytes,
			Valid: true,
		},
		SubscriptionTier: req.SubscriptionTier,
		ContactEmail:     req.ContactEmail,
		ParentID:         parentID,
		TicketID:         ticketID,
		Embedding:        embeddingVec,
	}

	_, err = s.queries.CreateProblemReport(ctx, params)
	if err != nil {
		return nil, fmt.Errorf("failed to create problem report: %w", err)
	}

	log.Info("problem report created",
		slog.String("report_id", reportID),
		slog.Bool("is_new_issue", isNewIssue))

	resp := &CreateProblemReportResponse{
		ID:         reportID,
		IsNewIssue: isNewIssue,
	}

	if ticketID != nil {
		resp.TicketID = *ticketID
	}
	if parentID != nil {
		resp.ParentID = *parentID
	}

	return resp, nil
}

func truncateString(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen-3] + "..."
}

func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func ptrToString(s *string, defaultVal string) string {
	if s == nil {
		return defaultVal
	}
	return *s
}
