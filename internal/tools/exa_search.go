package tools

import (
	"context"
	"fmt"
	"strings"

	"github.com/eternisai/enchanted-proxy/internal/logger"
	"github.com/eternisai/enchanted-proxy/internal/search"
)

// ExaSearchTool implements web search using Exa AI API.
type ExaSearchTool struct {
	searchService *search.Service
	logger        *logger.Logger
}

// NewExaSearchTool creates a new Exa search tool.
func NewExaSearchTool(searchService *search.Service, logger *logger.Logger) *ExaSearchTool {
	return &ExaSearchTool{
		searchService: searchService,
		logger:        logger,
	}
}

// Name returns the tool name.
func (t *ExaSearchTool) Name() string {
	return "web_search"
}

// Definition returns the OpenAI-compatible function definition.
func (t *ExaSearchTool) Definition() ToolDefinition {
	return ToolDefinition{
		Type: "function",
		Function: FunctionDef{
			Name:        "web_search",
			Description: "Run up to 3 search queries in one request",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"queries": map[string]interface{}{
						"type": "array",
						"items": map[string]interface{}{
							"type": "string",
						},
						"maxItems": 3,
					},
					"numResults": map[string]interface{}{
						"type":        "integer",
						"description": "Number of results to return (default: 10)",
					},
					"requires_live_results": map[string]interface{}{
						"type":        "boolean",
						"description": "Set to true when fresh/real-time data is needed. Examples: current stock prices, today's date or time, weather forecasts, live sports scores, breaking news. Default false uses cached results which is faster.",
					},
				},
				"required":             []string{"queries"},
				"additionalProperties": false,
			},
		},
	}
}

// ExaSearchArgs represents the arguments for Exa search.
type ExaSearchArgs struct {
	Queries            []string `json:"queries"`
	NumResults         int      `json:"numResults,omitempty"`
	RequiresLiveResults bool     `json:"requires_live_results,omitempty"`
}

// Execute runs the Exa search.
func (t *ExaSearchTool) Execute(ctx context.Context, args string) (string, error) {
	// Parse arguments
	var searchArgs ExaSearchArgs
	if err := ParseArguments(args, &searchArgs); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}

	if len(searchArgs.Queries) == 0 {
		return "", fmt.Errorf("queries is required")
	}

	// Limit to 3 queries
	if len(searchArgs.Queries) > 3 {
		searchArgs.Queries = searchArgs.Queries[:3]
	}

	// Set default numResults
	if searchArgs.NumResults == 0 {
		searchArgs.NumResults = 10
	}

	// Clamp to valid range (1-10)
	if searchArgs.NumResults < 1 {
		searchArgs.NumResults = 1
	}
	if searchArgs.NumResults > 10 {
		searchArgs.NumResults = 10
	}

	// Map boolean to Exa's livecrawl parameter
	// "preferred" tries live crawl first but falls back to cache on failure
	livecrawl := ""
	if searchArgs.RequiresLiveResults {
		livecrawl = "preferred"
	}

	t.logger.Info("executing web search",
		"queries", searchArgs.Queries,
		"num_results", searchArgs.NumResults,
		"requires_live_results", searchArgs.RequiresLiveResults)

	// Call search service
	searchReq := search.ExaSearchRequest{
		Queries:    searchArgs.Queries,
		NumResults: searchArgs.NumResults,
		Livecrawl:  livecrawl,
	}

	resp, err := t.searchService.SearchExa(ctx, searchReq)
	if err != nil {
		return "", fmt.Errorf("search failed: %w", err)
	}

	// Format results for AI consumption
	return t.formatResults(resp), nil
}

// formatResults formats search results as plain text for AI consumption.
// Matches iOS app format: "url: title. Snippet: summary"
func (t *ExaSearchTool) formatResults(resp *search.ExaSearchResponse) string {
	if len(resp.Results) == 0 {
		return "No search results found."
	}

	var formattedResults []string
	for _, r := range resp.Results {
		formatted := fmt.Sprintf("%s: %s. Snippet: %s", r.URL, r.Title, r.Summary)
		formattedResults = append(formattedResults, formatted)
	}

	return strings.Join(formattedResults, "\n")
}
