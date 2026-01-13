package tools

import (
	"context"
	"fmt"

	"github.com/eternisai/enchanted-proxy/internal/logger"
	pgdb "github.com/eternisai/enchanted-proxy/internal/storage/pg/sqlc"
	"github.com/google/uuid"
)

// FactType represents the type of fact being extracted.
type FactType string

const (
	FactTypeWorkContext     FactType = "work_context"
	FactTypePersonalContext FactType = "personal_context"
	FactTypeTopOfMind       FactType = "top_of_mind"
)

// FactExtractionTool extracts facts about the user during conversations.
type FactExtractionTool struct {
	queries *pgdb.Queries
	logger  *logger.Logger
}

// NewFactExtractionTool creates a new fact extraction tool.
func NewFactExtractionTool(queries *pgdb.Queries, log *logger.Logger) *FactExtractionTool {
	return &FactExtractionTool{
		queries: queries,
		logger:  log.WithComponent("fact-extraction-tool"),
	}
}

// Name returns the tool name.
func (t *FactExtractionTool) Name() string {
	return "extract_user_fact"
}

// Definition returns the OpenAI-compatible function definition.
func (t *FactExtractionTool) Definition() ToolDefinition {
	return ToolDefinition{
		Type: "function",
		Function: FunctionDef{
			Name:        "extract_user_fact",
			Description: "Extract and store facts about the user from the conversation. Use this to remember important information about the user's work, personal context, or current focus.",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"type": map[string]interface{}{
						"type": "string",
						"enum": []string{
							string(FactTypeWorkContext),
							string(FactTypePersonalContext),
							string(FactTypeTopOfMind),
						},
						"description": "Type of fact: work_context (role, industry, tools, expertise), personal_context (hobbies, interests, location, languages), top_of_mind (current projects, goals, problems)",
					},
					"text": map[string]interface{}{
						"type":        "string",
						"description": "The fact being extracted, written as a clear statement about the user",
					},
				},
				"required":             []string{"type", "text"},
				"additionalProperties": false,
			},
		},
	}
}

// FactExtractionArgs represents the arguments for fact extraction.
type FactExtractionArgs struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// Execute extracts and stores the fact in the database.
func (t *FactExtractionTool) Execute(ctx context.Context, args string) (string, error) {
	var factArgs FactExtractionArgs
	if err := ParseArguments(args, &factArgs); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}

	if factArgs.Type == "" {
		return "", fmt.Errorf("type is required")
	}

	if factArgs.Text == "" {
		return "", fmt.Errorf("text is required")
	}

	// Validate fact type
	validTypes := map[string]bool{
		string(FactTypeWorkContext):     true,
		string(FactTypePersonalContext): true,
		string(FactTypeTopOfMind):       true,
	}
	if !validTypes[factArgs.Type] {
		return "", fmt.Errorf("invalid fact type: %s", factArgs.Type)
	}

	// Extract user ID from context
	userID, ok := ctx.Value(logger.ContextKeyUserID).(string)
	if !ok || userID == "" {
		return "", fmt.Errorf("user not authenticated")
	}

	// Generate unique ID for the fact
	factID := uuid.New().String()

	// Log the extracted fact with full details
	t.logger.Info("=== FACT EXTRACTED ===",
		"fact_id", factID,
		"fact_type", factArgs.Type,
		"fact_text", factArgs.Text,
		"user_id", userID,
	)

	// Store the fact in the database
	_, err := t.queries.CreateUserFact(ctx, pgdb.CreateUserFactParams{
		ID:       factID,
		UserID:   userID,
		FactBody: factArgs.Text,
		FactType: factArgs.Type,
	})
	if err != nil {
		t.logger.Error("failed to store fact in database",
			"error", err.Error(),
			"fact_id", factID,
			"user_id", userID,
		)
		return "", fmt.Errorf("failed to store fact: %w", err)
	}

	// Log success with readable format
	t.logger.Info(fmt.Sprintf("[%s] %s (stored with ID: %s)", factArgs.Type, factArgs.Text, factID))

	return fmt.Sprintf("Fact stored: [%s] %s", factArgs.Type, factArgs.Text), nil
}
