package tools

import (
	"context"
	"encoding/json"
	"fmt"

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
	return "exa_search"
}

// Definition returns the OpenAI-compatible function definition.
func (t *ExaSearchTool) Definition() ToolDefinition {
	return ToolDefinition{
		Type: "function",
		Function: FunctionDef{
			Name:        "exa_search",
			Description: "Search the web using Exa AI to find current information, facts, articles, and research. Returns AI-generated summaries of search results.",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"query": map[string]interface{}{
						"type":        "string",
						"description": "The search query to find relevant web pages",
					},
					"num_results": map[string]interface{}{
						"type":        "integer",
						"description": "Number of results to return (1-10, default: 5)",
						"minimum":     1,
						"maximum":     10,
						"default":     5,
					},
				},
				"required": []string{"query"},
			},
		},
	}
}

// ExaSearchArgs represents the arguments for Exa search.
type ExaSearchArgs struct {
	Query      string `json:"query"`
	NumResults int    `json:"num_results,omitempty"`
}

// Execute runs the Exa search.
func (t *ExaSearchTool) Execute(ctx context.Context, args string) (string, error) {
	// Parse arguments
	var searchArgs ExaSearchArgs
	if err := ParseArguments(args, &searchArgs); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}

	if searchArgs.Query == "" {
		return "", fmt.Errorf("query is required")
	}

	// Set default num_results
	if searchArgs.NumResults == 0 {
		searchArgs.NumResults = 5
	}

	// Clamp to valid range
	if searchArgs.NumResults < 1 {
		searchArgs.NumResults = 1
	}
	if searchArgs.NumResults > 10 {
		searchArgs.NumResults = 10
	}

	t.logger.Info("executing exa search",
		"query", searchArgs.Query,
		"num_results", searchArgs.NumResults)

	// Call search service
	searchReq := search.ExaSearchRequest{
		Queries:    []string{searchArgs.Query},
		NumResults: searchArgs.NumResults,
	}

	resp, err := t.searchService.SearchExa(ctx, searchReq)
	if err != nil {
		return "", fmt.Errorf("search failed: %w", err)
	}

	// Format results for AI consumption
	return t.formatResults(resp), nil
}

// formatResults formats search results as a structured string for AI.
func (t *ExaSearchTool) formatResults(resp *search.ExaSearchResponse) string {
	if len(resp.Results) == 0 {
		return "No results found."
	}

	// Format as JSON for better structure
	type FormattedResult struct {
		Title         string `json:"title"`
		URL           string `json:"url"`
		Summary       string `json:"summary,omitempty"`
		PublishedDate string `json:"published_date,omitempty"`
		Author        string `json:"author,omitempty"`
	}

	results := make([]FormattedResult, len(resp.Results))
	for i, r := range resp.Results {
		results[i] = FormattedResult{
			Title:         r.Title,
			URL:           r.URL,
			Summary:       r.Summary,
			PublishedDate: r.PublishedDate,
			Author:        r.Author,
		}
	}

	output := map[string]interface{}{
		"query":         resp.Query,
		"results_count": len(results),
		"results":       results,
	}

	jsonBytes, err := json.MarshalIndent(output, "", "  ")
	if err != nil {
		return fmt.Sprintf("Error formatting results: %v", err)
	}
	return string(jsonBytes)
}
