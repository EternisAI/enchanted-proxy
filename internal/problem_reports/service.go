package problem_reports

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"

	"github.com/eternisai/enchanted-proxy/internal/logger"
	pgdb "github.com/eternisai/enchanted-proxy/internal/storage/pg/sqlc"
	"github.com/google/uuid"
)

var ErrMaxReportsReached = errors.New("maximum number of problem reports reached")

type Service struct {
	queries      *pgdb.Queries
	linearClient *LinearClient
	logger       *logger.Logger
}

func NewService(queries *pgdb.Queries, linearAPIKey, linearTeamID, linearProjectID, linearLabelID string, logger *logger.Logger) *Service {
	return &Service{
		queries:      queries,
		linearClient: NewLinearClient(linearAPIKey, linearTeamID, linearProjectID, linearLabelID),
		logger:       logger,
	}
}

func (s *Service) CreateReport(ctx context.Context, userID string, req *CreateProblemReportRequest) (*CreateProblemReportResponse, error) {
	log := s.logger.WithContext(ctx).WithComponent("problem-reports-service")

	count, err := s.queries.CountProblemReportsByUserID(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("failed to count user reports: %w", err)
	}

	if count >= MaxReportsPerUser {
		return nil, ErrMaxReportsReached
	}

	reportID := uuid.New().String()

	title := fmt.Sprintf("[Problem Report] %s", truncateString(req.ProblemDescription, 80))
	description := s.buildLinearDescription(req, reportID, userID)

	var ticketID *string
	linearTicketID, err := s.linearClient.CreateIssue(ctx, title, description)
	if err != nil {
		log.Error("failed to create Linear issue", slog.String("error", err.Error()))
	} else {
		ticketID = &linearTicketID
		log.Info("created Linear issue", slog.String("ticket_id", linearTicketID))
	}

	params := pgdb.CreateProblemReportParams{
		ID:                 reportID,
		UserID:             userID,
		ProblemDescription: req.ProblemDescription,
		SubscriptionTier:   req.SubscriptionTier,
		ContactEmail:       req.ContactEmail,
		TicketID:           ticketID,
	}

	if req.DeviceInfo != nil {
		params.DeviceModel = strPtr(req.DeviceInfo.DeviceModel)
		params.DeviceName = strPtr(req.DeviceInfo.DeviceName)
		params.SystemName = strPtr(req.DeviceInfo.SystemName)
		params.SystemVersion = strPtr(req.DeviceInfo.SystemVersion)
		params.AppVersion = strPtr(req.DeviceInfo.AppVersion)
		params.BuildNumber = strPtr(req.DeviceInfo.BuildNumber)
		params.Locale = strPtr(req.DeviceInfo.Locale)
		params.Timezone = strPtr(req.DeviceInfo.Timezone)
	}

	if req.StorageInfo != nil {
		params.TotalCapacityBytes = sql.NullInt64{Int64: req.StorageInfo.TotalCapacityBytes, Valid: true}
		params.AvailableCapacityBytes = sql.NullInt64{Int64: req.StorageInfo.AvailableCapacityBytes, Valid: true}
		params.UsedCapacityBytes = sql.NullInt64{Int64: req.StorageInfo.UsedCapacityBytes, Valid: true}
	}

	_, err = s.queries.CreateProblemReport(ctx, params)
	if err != nil {
		return nil, fmt.Errorf("failed to create problem report: %w", err)
	}

	log.Info("problem report created", slog.String("report_id", reportID))

	resp := &CreateProblemReportResponse{
		ID: reportID,
	}
	if ticketID != nil {
		resp.TicketID = *ticketID
	}

	return resp, nil
}

func (s *Service) buildLinearDescription(req *CreateProblemReportRequest, reportID, userID string) string {
	desc := fmt.Sprintf("**Description:**\n%s\n\n", req.ProblemDescription)

	if req.DeviceInfo != nil {
		desc += fmt.Sprintf("**Device:** %s (%s %s)\n**App Version:** %s (%s)\n**Locale:** %s\n**Timezone:** %s\n\n",
			req.DeviceInfo.DeviceModel,
			req.DeviceInfo.SystemName,
			req.DeviceInfo.SystemVersion,
			req.DeviceInfo.AppVersion,
			req.DeviceInfo.BuildNumber,
			req.DeviceInfo.Locale,
			req.DeviceInfo.Timezone)
	} else {
		desc += "**Device Info:** not provided (user opted out)\n\n"
	}

	if req.StorageInfo != nil {
		desc += fmt.Sprintf("**Storage:** %.1f GB free / %.1f GB total\n\n",
			float64(req.StorageInfo.AvailableCapacityBytes)/(1024*1024*1024),
			float64(req.StorageInfo.TotalCapacityBytes)/(1024*1024*1024))
	}

	desc += fmt.Sprintf("**Subscription Tier:** %s\n**Contact Email:** %s\n\n**Report ID:** %s\n**User ID:** %s",
		ptrToString(req.SubscriptionTier, "unknown"),
		ptrToString(req.ContactEmail, "not provided"),
		reportID,
		userID)

	return desc
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
