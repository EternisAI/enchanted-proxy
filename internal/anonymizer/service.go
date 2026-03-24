package anonymizer

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// Service orchestrates anonymization of user messages.
type Service struct {
	client *Client
}

// NewService creates a new anonymizer service.
func NewService(client *Client) *Service {
	return &Service{client: client}
}

// AnonymizeResult holds the result of anonymizing a message.
type AnonymizeResult struct {
	Text         string        // The anonymized message text
	Replacements []Replacement // The PII replacements that were applied
}

// Anonymize sends the user message to the anonymizer model and returns the
// anonymized text along with the replacement map.
// If no PII is found, returns the original text with an empty replacement list.
func (s *Service) Anonymize(ctx context.Context, userMessage string) (*AnonymizeResult, error) {
	content, err := s.client.Call(ctx, userMessage)
	if err != nil {
		return nil, fmt.Errorf("anonymizer call failed: %w", err)
	}

	replacements, err := ParseResponse(content)
	if err != nil {
		return nil, fmt.Errorf("failed to parse anonymizer response: %w", err)
	}

	if len(replacements) == 0 {
		return &AnonymizeResult{Text: userMessage}, nil
	}

	anonymized := ApplyReplacements(userMessage, replacements)

	return &AnonymizeResult{
		Text:         anonymized,
		Replacements: replacements,
	}, nil
}

// ApplyReplacements substitutes all original PII strings with their replacements.
// Replacements are applied longest-first to avoid partial matches.
func ApplyReplacements(text string, replacements []Replacement) string {
	// Sort by original length descending so longer matches are replaced first
	sorted := make([]Replacement, len(replacements))
	copy(sorted, replacements)
	sort.Slice(sorted, func(i, j int) bool {
		return len(sorted[i].Original) > len(sorted[j].Original)
	})

	for _, r := range sorted {
		text = strings.ReplaceAll(text, r.Original, r.Replacement)
	}
	return text
}
