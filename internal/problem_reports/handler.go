package problem_reports

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/eternisai/enchanted-proxy/internal/auth"
	"github.com/eternisai/enchanted-proxy/internal/logger"
	"github.com/gin-gonic/gin"
)

type Handler struct {
	service *Service
	logger  *logger.Logger
}

func NewHandler(service *Service, logger *logger.Logger) *Handler {
	return &Handler{
		service: service,
		logger:  logger,
	}
}

func (h *Handler) CreateProblemReport(c *gin.Context) {
	log := h.logger.WithContext(c.Request.Context()).WithComponent("problem-reports-handler")

	userID, ok := auth.GetUserID(c)
	if !ok {
		log.Error("user not authenticated")
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}

	var req CreateProblemReportRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		log.Error("failed to bind request", slog.String("error", err.Error()))
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body", "details": err.Error()})
		return
	}

	if strings.TrimSpace(req.ProblemDescription) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "problemDescription is required"})
		return
	}

	resp, err := h.service.CreateReport(c.Request.Context(), userID, &req)
	if err != nil {
		log.Error("failed to create problem report",
			slog.String("error", err.Error()),
			slog.String("user_id", userID))

		if errors.Is(err, ErrMaxReportsReached) {
			c.JSON(http.StatusTooManyRequests, gin.H{"error": "maximum number of problem reports reached"})
			return
		}

		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create problem report"})
		return
	}

	log.Info("problem report created",
		slog.String("report_id", resp.ID),
		slog.String("user_id", userID))

	c.JSON(http.StatusCreated, resp)
}
