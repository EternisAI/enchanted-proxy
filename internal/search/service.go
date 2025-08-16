package search

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/eternisai/enchanted-proxy/internal/config"
	"github.com/eternisai/enchanted-proxy/internal/logger"
)

// Service handles search operations
type Service struct {
	httpClient *http.Client
	logger     *logger.Logger
	serpAPIKey string
}

// NewService creates a new search service
func NewService(logger *logger.Logger) *Service {
	return &Service{
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
		logger:     logger,
		serpAPIKey: config.AppConfig.SerpAPIKey,
	}
}

// SearchRequest represents a search request from the client
type SearchRequest struct {
	Query      string `json:"query" binding:"required"`
	Engine     string `json:"engine,omitempty"`     // default: "duckduckgo"
	TimeFilter string `json:"time_filter,omitempty"` // "d", "w", "m", "y"
}

// SearchResponse represents the standardized search response
type SearchResponse struct {
	Query           string         `json:"query"`
	Engine          string         `json:"engine"`
	OrganicResults  []SearchResult `json:"organic_results"`
	RelatedQueries  []string       `json:"related_queries,omitempty"`
	SearchMetadata  SearchMetadata `json:"search_metadata"`
	ProcessingTime  string         `json:"processing_time"`
}

// SearchResult represents a single search result
type SearchResult struct {
	Position int    `json:"position"`
	Title    string `json:"title"`
	Link     string `json:"link"`
	Snippet  string `json:"snippet"`
	Source   string `json:"source,omitempty"`
}

// SearchMetadata contains metadata about the search
type SearchMetadata struct {
	TotalResults string `json:"total_results,omitempty"`
	TimeTaken    string `json:"time_taken,omitempty"`
	Engine       string `json:"engine"`
	Status       string `json:"status"`
}

// SerpAPIDuckDuckGoResponse represents the raw SerpAPI DuckDuckGo response
type SerpAPIDuckDuckGoResponse struct {
	OrganicResults []struct {
		Position int    `json:"position"`
		Title    string `json:"title"`
		Link     string `json:"link"`
		Snippet  string `json:"snippet"`
	} `json:"organic_results"`
	RelatedSearches []struct {
		Query string `json:"query"`
	} `json:"related_searches"`
	SearchMetadata struct {
		Status         string `json:"status"`
		ProcessedAt    string `json:"processed_at"`
		TotalTimeTaken float64 `json:"total_time_taken"`
	} `json:"search_metadata"`
	Error string `json:"error,omitempty"`
}

// SearchDuckDuckGo performs a DuckDuckGo search via SerpAPI
func (s *Service) SearchDuckDuckGo(ctx context.Context, req SearchRequest) (*SearchResponse, error) {
	start := time.Now()
	
	if s.serpAPIKey == "" {
		return nil, fmt.Errorf("SerpAPI key not configured")
	}

	// Build SerpAPI request URL
	apiURL, err := s.buildSerpAPIURL(req)
	if err != nil {
		return nil, fmt.Errorf("failed to build API URL: %w", err)
	}

	// Make HTTP request
	httpReq, err := http.NewRequestWithContext(ctx, "GET", apiURL, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	resp, err := s.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("failed to make request: %w", err)
	}
	defer resp.Body.Close()

	// Read response body
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	// Check for HTTP errors
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("SerpAPI returned status %d: %s", resp.StatusCode, string(body))
	}

	// Parse SerpAPI response
	var serpResp SerpAPIDuckDuckGoResponse
	if err := json.Unmarshal(body, &serpResp); err != nil {
		return nil, fmt.Errorf("failed to parse SerpAPI response: %w", err)
	}

	// Check for API errors
	if serpResp.Error != "" {
		return nil, fmt.Errorf("SerpAPI error: %s", serpResp.Error)
	}

	// Convert to standardized response
	searchResp := s.convertSerpAPIResponse(req, serpResp, time.Since(start))

	return searchResp, nil
}

// buildSerpAPIURL constructs the SerpAPI request URL
func (s *Service) buildSerpAPIURL(req SearchRequest) (string, error) {
	baseURL := "https://serpapi.com/search.json"
	
	params := url.Values{}
	params.Set("api_key", s.serpAPIKey)
	params.Set("engine", "duckduckgo")
	params.Set("q", req.Query)

	// Set count (results per page) - always use default of 10
	count := 10
	params.Set("s", fmt.Sprintf("%d", count))

	// Always use US English settings
	params.Set("kl", "us-en")    // Language: US English
	params.Set("safe_search", "0") // Safe search: moderate
	params.Set("region", "us-en")  // Region: US English

	// Set time filter if provided
	if req.TimeFilter != "" {
		params.Set("time", req.TimeFilter)
	}

	return baseURL + "?" + params.Encode(), nil
}

// convertSerpAPIResponse converts SerpAPI response to standardized format
func (s *Service) convertSerpAPIResponse(req SearchRequest, serpResp SerpAPIDuckDuckGoResponse, processingTime time.Duration) *SearchResponse {
	// Convert organic results
	results := make([]SearchResult, 0, len(serpResp.OrganicResults))
	for _, result := range serpResp.OrganicResults {
		results = append(results, SearchResult{
			Position: result.Position,
			Title:    result.Title,
			Link:     result.Link,
			Snippet:  result.Snippet,
			Source:   extractDomain(result.Link),
		})
	}

	// Convert related queries
	relatedQueries := make([]string, 0, len(serpResp.RelatedSearches))
	for _, related := range serpResp.RelatedSearches {
		if related.Query != "" {
			relatedQueries = append(relatedQueries, related.Query)
		}
	}

	// Build response
	engine := req.Engine
	if engine == "" {
		engine = "duckduckgo"
	}

	return &SearchResponse{
		Query:          req.Query,
		Engine:         engine,
		OrganicResults: results,
		RelatedQueries: relatedQueries,
		SearchMetadata: SearchMetadata{
			Engine: engine,
			Status: serpResp.SearchMetadata.Status,
			TimeTaken: fmt.Sprintf("%.2fs", serpResp.SearchMetadata.TotalTimeTaken),
		},
		ProcessingTime: fmt.Sprintf("%.2fms", float64(processingTime.Nanoseconds())/1000000),
	}
}

// extractDomain extracts domain from URL for display
func extractDomain(urlStr string) string {
	if u, err := url.Parse(urlStr); err == nil {
		return u.Host
	}
	return ""
}
