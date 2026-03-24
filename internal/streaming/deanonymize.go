package streaming

import (
	"encoding/json"
	"sort"
	"strings"
)

// Deanonymizer reverses anonymizer replacements in AI response text.
// It replaces anonymized tokens (e.g., "123 Pear St") back to originals ("123 Apple St").
// Case-insensitive matching handles LLM variations in casing.
type Deanonymizer struct {
	// pairs sorted longest-replacement-first to avoid partial matches
	pairs []replacementPair
}

type replacementPair struct {
	anonymized string // what the LLM sees/outputs (lowercase for matching)
	original   string // what the user originally wrote
}

// NewDeanonymizer creates a deanonymizer from the anonymizer's replacement list.
// replacementsJSON is the JSON-encoded []{"original":"...","replacement":"..."} array.
// Returns nil if the JSON is empty or invalid.
func NewDeanonymizer(replacementsJSON string) *Deanonymizer {
	if replacementsJSON == "" {
		return nil
	}

	var replacements []struct {
		Original    string `json:"original"`
		Replacement string `json:"replacement"`
	}
	if err := json.Unmarshal([]byte(replacementsJSON), &replacements); err != nil {
		return nil
	}
	if len(replacements) == 0 {
		return nil
	}

	pairs := make([]replacementPair, 0, len(replacements))
	for _, r := range replacements {
		if r.Replacement != "" {
			pairs = append(pairs, replacementPair{
				anonymized: strings.ToLower(r.Replacement),
				original:   r.Original,
			})
		}
	}

	// Sort longest first to prevent partial matches
	sort.Slice(pairs, func(i, j int) bool {
		return len(pairs[i].anonymized) > len(pairs[j].anonymized)
	})

	return &Deanonymizer{pairs: pairs}
}

// ReplaceInSSELine replaces anonymized tokens in the content delta of an SSE data line.
// Only modifies the "content" field inside choices[0].delta to avoid corrupting other JSON fields.
// Returns the (possibly modified) line.
func (d *Deanonymizer) ReplaceInSSELine(line string) string {
	if !strings.HasPrefix(line, "data: ") {
		return line
	}

	data := strings.TrimPrefix(line, "data: ")
	if data == "[DONE]" {
		return line
	}

	var parsed map[string]interface{}
	if err := json.Unmarshal([]byte(data), &parsed); err != nil {
		return line
	}

	choices, ok := parsed["choices"].([]interface{})
	if !ok || len(choices) == 0 {
		return line
	}

	firstChoice, ok := choices[0].(map[string]interface{})
	if !ok {
		return line
	}

	delta, ok := firstChoice["delta"].(map[string]interface{})
	if !ok {
		return line
	}

	content, ok := delta["content"].(string)
	if !ok || content == "" {
		return line
	}

	replaced := d.replaceAll(content)
	if replaced == content {
		return line
	}

	delta["content"] = replaced
	newData, err := json.Marshal(parsed)
	if err != nil {
		return line
	}

	return "data: " + string(newData)
}

// ReplaceInText replaces anonymized tokens in plain text (for non-streaming responses).
func (d *Deanonymizer) ReplaceInText(text string) string {
	return d.replaceAll(text)
}

// replaceAll performs case-insensitive replacement of all anonymized tokens.
func (d *Deanonymizer) replaceAll(text string) string {
	lower := strings.ToLower(text)
	var b strings.Builder
	b.Grow(len(text))

	i := 0
	for i < len(text) {
		matched := false
		for _, p := range d.pairs {
			if i+len(p.anonymized) <= len(lower) && lower[i:i+len(p.anonymized)] == p.anonymized {
				b.WriteString(p.original)
				i += len(p.anonymized)
				matched = true
				break
			}
		}
		if !matched {
			b.WriteByte(text[i])
			i++
		}
	}

	return b.String()
}
