package search

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/eternisai/enchanted-proxy/internal/config"
	"github.com/eternisai/enchanted-proxy/internal/logger"
)

// Service handles search operations.
type Service struct {
	httpClient *http.Client
	logger     *logger.Logger
	serpAPIKey string
	exaAPIKey  string
}

// NewService creates a new search service.
func NewService(logger *logger.Logger) *Service {
	return &Service{
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
		logger:     logger,
		serpAPIKey: config.AppConfig.SerpAPIKey,
		exaAPIKey:  config.AppConfig.ExaAPIKey,
	}
}

// SearchRequest represents a search request from the client.
type SearchRequest struct {
	Query      string `json:"query" binding:"required"`
	Engine     string `json:"engine,omitempty"`      // default: "duckduckgo"
	TimeFilter string `json:"time_filter,omitempty"` // "d", "w", "m", "y"
}

// ExaSearchRequest represents a search request for Exa API.
type ExaSearchRequest struct {
	Queries    []string `json:"queries,omitempty"`
	Query      string   `json:"query,omitempty"`
	NumResults int      `json:"num_results,omitempty"` // default: 10, max: 10
	Livecrawl  string   `json:"livecrawl,omitempty"`   // "never", "fallback", "preferred", "always"
}

// SearchResponse represents the standardized search response.
type SearchResponse struct {
	Query          string         `json:"query"`
	Engine         string         `json:"engine"`
	OrganicResults []SearchResult `json:"organic_results"`
	RelatedQueries []string       `json:"related_queries,omitempty"`
	SearchMetadata SearchMetadata `json:"search_metadata"`
	ProcessingTime string         `json:"processing_time"`
}

// SearchResult represents a single search result.
type SearchResult struct {
	Position int    `json:"position"`
	Title    string `json:"title"`
	Link     string `json:"link"`
	Snippet  string `json:"snippet"`
	Source   string `json:"source,omitempty"`
}

// ExaSearchResult represents a single Exa search result.
type ExaSearchResult struct {
	URL           string `json:"url"`
	Title         string `json:"title"`
	PublishedDate string `json:"published_date,omitempty"`
	Author        string `json:"author,omitempty"`
	Summary       string `json:"summary,omitempty"` // AI-generated summary if requested
	Image         string `json:"image,omitempty"`   // featured image URL
	Favicon       string `json:"favicon,omitempty"` // favicon URL
}

// SearchMetadata contains metadata about the search.
type SearchMetadata struct {
	TotalResults string `json:"total_results,omitempty"`
	TimeTaken    string `json:"time_taken,omitempty"`
	Engine       string `json:"engine"`
	Status       string `json:"status"`
}

// ExaSearchResponse represents the response from Exa search.
type ExaSearchResponse struct {
	Query          string            `json:"query"`
	Results        []ExaSearchResult `json:"results"`
	ProcessingTime string            `json:"processing_time"`
	SearchMetadata ExaSearchMetadata `json:"search_metadata"`
}

// ExaSearchMetadata contains metadata about the Exa search.
type ExaSearchMetadata struct {
	Engine       string `json:"engine"`
	Status       string `json:"status"`
	ResultsCount int    `json:"results_count"`
	ResponseTime string `json:"response_time"`
}

// SerpAPIDuckDuckGoResponse represents the raw SerpAPI DuckDuckGo response.
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
		Status         string  `json:"status"`
		ProcessedAt    string  `json:"processed_at"`
		TotalTimeTaken float64 `json:"total_time_taken"`
	} `json:"search_metadata"`
	Error string `json:"error,omitempty"`
}

// ExaAPIResponse represents the raw response from Exa API.
type ExaAPIResponse struct {
	Results []struct {
		ID            string  `json:"id"`
		URL           string  `json:"url"`
		Title         string  `json:"title"`
		Score         float64 `json:"score,omitempty"`
		PublishedDate string  `json:"publishedDate,omitempty"`
		Author        string  `json:"author,omitempty"`
		Text          string  `json:"text,omitempty"`
		Summary       string  `json:"summary,omitempty"`
		Image         string  `json:"image,omitempty"`
		Favicon       string  `json:"favicon,omitempty"`
	} `json:"results"`
	AutoPromptString string `json:"autopromptString,omitempty"`
	RequestID        string `json:"requestId,omitempty"`
}

// SearchDuckDuckGo performs a DuckDuckGo search via SerpAPI.
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
	defer resp.Body.Close() //nolint:errcheck

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

// SearchExa performs parallel searches using Exa AI API.
func (s *Service) SearchExa(ctx context.Context, req ExaSearchRequest) (*ExaSearchResponse, error) {
	start := time.Now()

	if s.exaAPIKey == "" {
		return nil, fmt.Errorf("failed to create request: Exa API key not configured")
	}

	if len(req.Queries) == 0 {
		return nil, fmt.Errorf("at least one query is required")
	}

	// Create channels for results and errors
	type searchResult struct {
		query   string
		results []ExaSearchResult
		err     error
	}
	resultChan := make(chan searchResult, len(req.Queries))

	// Launch parallel searches
	for _, query := range req.Queries {
		go func(q string) {
			// Build Exa API request payload
			payload, err := s.buildExaAPIPayload(ExaSearchRequest{
				Queries:    []string{q},
				NumResults: req.NumResults,
				Livecrawl:  req.Livecrawl,
			})
			if err != nil {
				resultChan <- searchResult{query: q, err: fmt.Errorf("failed to build API payload: %w", err)}
				return
			}

			// Make HTTP request
			httpReq, err := http.NewRequestWithContext(ctx, "POST", "https://api.exa.ai/search", bytes.NewBuffer(payload))
			if err != nil {
				resultChan <- searchResult{query: q, err: fmt.Errorf("failed to create request: %w", err)}
				return
			}

			httpReq.Header.Set("Content-Type", "application/json")
			httpReq.Header.Set("x-api-key", s.exaAPIKey)

			resp, err := s.httpClient.Do(httpReq)
			if err != nil {
				resultChan <- searchResult{query: q, err: fmt.Errorf("failed to make request: %w", err)}
				return
			}
			defer resp.Body.Close()

			// Read response body
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				resultChan <- searchResult{query: q, err: fmt.Errorf("failed to read response: %w", err)}
				return
			}

			// Check for HTTP errors
			if resp.StatusCode != http.StatusOK {
				resultChan <- searchResult{query: q, err: fmt.Errorf("Exa API returned status %d: %s", resp.StatusCode, string(body))}
				return
			}

			// Parse Exa API response
			var exaResp ExaAPIResponse
			if err := json.Unmarshal(body, &exaResp); err != nil {
				resultChan <- searchResult{query: q, err: fmt.Errorf("failed to parse Exa API response: %w", err)}
				return
			}

			// Convert results
			results := make([]ExaSearchResult, 0, len(exaResp.Results))
			for _, result := range exaResp.Results {
				results = append(results, ExaSearchResult{
					URL:           result.URL,
					Title:         result.Title,
					PublishedDate: result.PublishedDate,
					Author:        result.Author,
					Summary:       result.Summary,
					Image:         result.Image,
					Favicon:       result.Favicon,
				})
			}

			resultChan <- searchResult{query: q, results: results}
		}(query)
	}

	// Collect results
	var allResults []ExaSearchResult
	var errors []error
	for i := 0; i < len(req.Queries); i++ {
		result := <-resultChan
		if result.err != nil {
			errors = append(errors, fmt.Errorf("query '%s': %w", result.query, result.err))
		} else {
			allResults = append(allResults, result.results...)
		}
	}

	// If all queries failed, return error
	if len(errors) == len(req.Queries) {
		return nil, fmt.Errorf("all queries failed: %v", errors)
	}

	// Build combined response
	return &ExaSearchResponse{
		Query:          strings.Join(req.Queries, ", "),
		Results:        allResults,
		ProcessingTime: fmt.Sprintf("%.2fms", float64(time.Since(start).Nanoseconds())/1000000),
		SearchMetadata: ExaSearchMetadata{
			Engine:       "exa",
			Status:       "success",
			ResultsCount: len(allResults),
			ResponseTime: fmt.Sprintf("%.2fms", float64(time.Since(start).Nanoseconds())/1000000),
		},
	}, nil
}

// buildSerpAPIURL constructs the SerpAPI request URL.
func (s *Service) buildSerpAPIURL(req SearchRequest) (string, error) {
	baseURL := "https://serpapi.com/search.json"

	params := url.Values{}
	params.Set("api_key", s.serpAPIKey)
	params.Set("engine", "duckduckgo")
	params.Set("q", req.Query)

	// Always use US English settings
	params.Set("kl", "us-en")      // Language/locale: US English (covers region)
	params.Set("safe", "-1")       // Safe search: moderate (-1=moderate, 1=strict, -2=off)
	params.Set("no_cache", "true") // Zero trace: prevent caching for privacy

	// Set time filter if provided
	if req.TimeFilter != "" {
		params.Set("time", req.TimeFilter)
	}

	return baseURL + "?" + params.Encode(), nil
}

// convertSerpAPIResponse converts SerpAPI response to standardized format.
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
			Engine:    engine,
			Status:    serpResp.SearchMetadata.Status,
			TimeTaken: fmt.Sprintf("%.2fs", serpResp.SearchMetadata.TotalTimeTaken),
		},
		ProcessingTime: fmt.Sprintf("%.2fms", float64(processingTime.Nanoseconds())/1000000),
	}
}

// buildExaAPIPayload constructs the Exa API request payload.
func (s *Service) buildExaAPIPayload(req ExaSearchRequest) ([]byte, error) {
	if len(req.Queries) == 0 {
		return nil, fmt.Errorf("at least one query is required")
	}

	payload := map[string]interface{}{
		"query": req.Queries[0], // Use first query since this is called per query
		"type":  "deep",         // use deep search type (faster and similar quality to keyword)
	}

	// Set number of results (default 10, max 10)
	numResults := req.NumResults
	if numResults <= 0 {
		numResults = 10
	}
	if numResults > 10 {
		numResults = 10
	}
	payload["numResults"] = numResults

	// Configure content options - use Exa's built-in summary instead of custom prompt
	contents := map[string]interface{}{
		"summary": true, // Use Exa's default summary generation
	}

	// Add livecrawl option if specified (controls caching behavior)
	// Options: "never" (default, use cache), "fallback", "preferred", "always"
	if req.Livecrawl != "" {
		contents["livecrawl"] = req.Livecrawl
	}

	payload["contents"] = contents

	return json.Marshal(payload)
}

// convertExaAPIResponse converts Exa API response to standardized format.
func (s *Service) convertExaAPIResponse(req ExaSearchRequest, exaResp ExaAPIResponse, processingTime time.Duration) *ExaSearchResponse {
	// Convert results
	results := make([]ExaSearchResult, 0, len(exaResp.Results))
	for _, result := range exaResp.Results {
		results = append(results, ExaSearchResult{
			URL:           result.URL,
			Title:         result.Title,
			PublishedDate: result.PublishedDate,
			Author:        result.Author,
			Summary:       result.Summary,
			Image:         result.Image,
			Favicon:       result.Favicon,
		})
	}

	// Build response
	return &ExaSearchResponse{
		Query:          strings.Join(req.Queries, ", "),
		Results:        results,
		ProcessingTime: fmt.Sprintf("%.2fms", float64(processingTime.Nanoseconds())/1000000),
		SearchMetadata: ExaSearchMetadata{
			Engine:       "exa",
			Status:       "success",
			ResultsCount: len(results),
			ResponseTime: fmt.Sprintf("%.2fms", float64(processingTime.Nanoseconds())/1000000),
		},
	}
}

// extractDomain extracts domain from URL for display.
func extractDomain(urlStr string) string {
	if u, err := url.Parse(urlStr); err == nil {
		return u.Host
	}
	return ""
}
