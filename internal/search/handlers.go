package search

import (
	"context"
	"log/slog"
	"net/http"
	"strings"

	"github.com/eternisai/enchanted-proxy/internal/auth"
	"github.com/eternisai/enchanted-proxy/internal/logger"
	"github.com/gin-gonic/gin"
)

// SearchService interface defines the methods needed by the handler.
type SearchService interface {
	SearchDuckDuckGo(ctx context.Context, req SearchRequest) (*SearchResponse, error)
	SearchExa(ctx context.Context, req ExaSearchRequest) (*ExaSearchResponse, error)
}

// Handler handles HTTP requests for search operations.
type Handler struct {
	service SearchService
	logger  *logger.Logger
}

// NewHandler creates a new search handler.
func NewHandler(service *Service, logger *logger.Logger) *Handler {
	return &Handler{
		service: service,
		logger:  logger,
	}
}

// PostSearchHandler handles POST /api/search requests with JSON body.
func (h *Handler) PostSearchHandler(c *gin.Context) {
	log := h.logger.WithContext(c.Request.Context()).WithComponent("search_handler")

	// Get user ID from auth context for logging
	userID, _ := auth.GetUserUUID(c)

	var searchReq SearchRequest
	if err := c.ShouldBindJSON(&searchReq); err != nil {
		log.Warn("invalid search request body",
			slog.String("error", err.Error()),
			slog.String("user_id", userID))
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "Invalid request body: " + err.Error(),
		})
		return
	}

	// Validate required fields
	searchReq.Query = strings.TrimSpace(searchReq.Query)
	if searchReq.Query == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "Missing required field 'query'",
		})
		return
	}

	// Set defaults
	if searchReq.Engine == "" {
		searchReq.Engine = "duckduckgo"
	}

	// Validate engine
	if searchReq.Engine != "duckduckgo" {
		log.Warn("unsupported search engine requested",
			slog.String("engine", searchReq.Engine),
			slog.String("user_id", userID))
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "Unsupported search engine. Currently supported: 'duckduckgo'",
		})
		return
	}

	log.Info("processing search request",
		slog.String("engine", searchReq.Engine),
		slog.String("user_id", userID))

	// Log query at debug level for troubleshooting (if needed)
	log.Debug("search query details",
		slog.String("query", searchReq.Query),
		slog.String("user_id", userID))

	// Perform search
	result, err := h.service.SearchDuckDuckGo(c.Request.Context(), searchReq)
	if err != nil {
		log.Error("search request failed",
			slog.String("engine", searchReq.Engine),
			slog.String("error", err.Error()),
			slog.String("user_id", userID))

		// Log query at debug level for troubleshooting
		log.Debug("failed search query details",
			slog.String("query", "[REDACTED]"),
			slog.String("user_id", userID))

		c.JSON(http.StatusInternalServerError, gin.H{
			"error": "Search request failed",
		})
		return
	}

	log.Info("search request completed",
		slog.Int("results_count", len(result.OrganicResults)),
		slog.String("processing_time", result.ProcessingTime),
		slog.String("user_id", userID))

	c.JSON(http.StatusOK, result)
}

// PostExaSearchHandler handles POST /api/exa/search requests with JSON body.
func (h *Handler) PostExaSearchHandler(c *gin.Context) {
	log := h.logger.WithContext(c.Request.Context()).WithComponent("exa_search_handler")

	// Get user ID from auth context for logging
	userID, _ := auth.GetUserUUID(c)

	var searchReq ExaSearchRequest
	if err := c.ShouldBindJSON(&searchReq); err != nil {
		log.Warn("invalid exa search request body",
			slog.String("error", err.Error()),
			slog.String("user_id", userID))
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "Invalid request body: " + err.Error(),
		})
		return
	}

	// Validate required fields
	searchReq.Query = strings.TrimSpace(searchReq.Query)
	if searchReq.Query == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "Missing required field 'query'",
		})
		return
	}

	// Set defaults
	if searchReq.NumResults <= 0 {
		searchReq.NumResults = 10
	}
	if searchReq.NumResults > 10 {
		searchReq.NumResults = 10 // Exa API limit
	}

	log.Info("processing exa search request",
		slog.Int("num_results", searchReq.NumResults),
		slog.String("user_id", userID))

	// Log query at debug level for troubleshooting (if needed)
	log.Debug("exa search query details",
		slog.String("query", searchReq.Query),
		slog.String("user_id", userID))

	// Perform Exa search
	result, err := h.service.SearchExa(c.Request.Context(), searchReq)
	if err != nil {
		log.Error("exa search request failed",
			slog.String("error", err.Error()),
			slog.String("user_id", userID))

		// Log query at debug level for troubleshooting
		log.Debug("failed exa search query details",
			slog.String("query", "[REDACTED]"),
			slog.String("user_id", userID))

		c.JSON(http.StatusInternalServerError, gin.H{
			"error": "Exa search request failed",
		})
		return
	}

	log.Info("exa search request completed",
		slog.Int("results_count", len(result.Results)),
		slog.String("processing_time", result.ProcessingTime),
		slog.String("user_id", userID))

	c.JSON(http.StatusOK, result)
}
