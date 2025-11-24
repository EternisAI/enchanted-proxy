package tools

import (
	"context"
	"fmt"
	"strings"

	"github.com/eternisai/enchanted-proxy/internal/logger"
	"github.com/eternisai/enchanted-proxy/internal/search"
)

// ExaSearchTool provides web search capabilities using Exa AI API.
type ExaSearchTool struct {
	searchService *search.Service
	logger        *logger.Logger
}

// NewExaSearchTool creates a new Exa search tool.
func NewExaSearchTool(searchService *search.Service, logger *logger.Logger) *ExaSearchTool {
	return &ExaSearchTool{
		searchService: searchService,
		logger:        logger.WithComponent("exa-search-tool"),
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
			Description: "Run up to 3 search queries in one request to find current information from the web",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"queries": map[string]interface{}{
						"type":        "array",
						"items":       map[string]interface{}{"type": "string"},
						"maxItems":    3,
						"description": "Search queries to execute (up to 3 queries)",
					},
					"numResults": map[string]interface{}{
						"type":        "integer",
						"description": "Number of results to return per query (default: 10)",
						"default":     10,
					},
				},
				"required":             []string{"queries"},
				"additionalProperties": false,
			},
		},
	}
}

// searchArgs represents the parsed arguments for web search.
type searchArgs struct {
	Queries    []string `json:"queries"`
	NumResults int      `json:"numResults"`
}

// Execute runs the web search with the given arguments.
func (t *ExaSearchTool) Execute(ctx context.Context, args string) (string, error) {
	// Parse arguments
	var searchArgs searchArgs
	if err := ParseArguments(args, &searchArgs); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}

	// Validate queries
	if len(searchArgs.Queries) == 0 {
		return "", fmt.Errorf("queries parameter is required and must be a non-empty array")
	}

	if len(searchArgs.Queries) > 3 {
		return "", fmt.Errorf("maximum 3 queries allowed")
	}

	// Validate all queries are strings
	for i, q := range searchArgs.Queries {
		if strings.TrimSpace(q) == "" {
			return "", fmt.Errorf("query at index %d is empty", i)
		}
		searchArgs.Queries[i] = strings.TrimSpace(q)
	}

	// Set default numResults
	if searchArgs.NumResults <= 0 {
		searchArgs.NumResults = 10
	}
	if searchArgs.NumResults > 10 {
		searchArgs.NumResults = 10
	}

	t.logger.Info("executing web search",
		"num_queries", len(searchArgs.Queries),
		"num_results", searchArgs.NumResults)

	// Call search service
	result, err := t.searchService.SearchExa(ctx, search.ExaSearchRequest{
		Queries:    searchArgs.Queries,
		NumResults: searchArgs.NumResults,
	})

	if err != nil {
		t.logger.Error("search failed", "error", err.Error())
		return "", fmt.Errorf("search failed: %w", err)
	}

	// Handle no results
	if len(result.Results) == 0 {
		t.logger.Info("no search results found")
		return "No search results found.", nil
	}

	// Format results for AI consumption
	formatted := t.formatResults(result.Results)

	t.logger.Info("search completed",
		"results_count", len(result.Results),
		"processing_time", result.ProcessingTime)

	return formatted, nil
}

// formatResults formats search results for AI consumption.
func (t *ExaSearchTool) formatResults(results []search.ExaSearchResult) string {
	var builder strings.Builder

	for i, result := range results {
		if i > 0 {
			builder.WriteString("\n\n")
		}

		// Format: URL: Title. Snippet: Summary
		builder.WriteString(result.URL)
		builder.WriteString(": ")
		builder.WriteString(strings.TrimSpace(result.Title))
		builder.WriteString(". Snippet: ")
		builder.WriteString(strings.TrimSpace(result.Summary))
	}

	return builder.String()
}
