package anonymizer

import (
	"testing"
)

func TestParseResponse_WithReplacements(t *testing.T) {
	content := `<think>

</think>

<tool_call>
{"name": "replace_entities", "arguments": {"replacements": [{"original": "Elijah", "replacement": "Alex"}, {"original": "TechStartup Inc", "replacement": "NovoTech Solutions"}, {"original": "$85,000", "replacement": "$90,000"}]}}
</tool_call>`

	replacements, err := ParseResponse(content)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(replacements) != 3 {
		t.Fatalf("expected 3 replacements, got %d", len(replacements))
	}

	expected := []Replacement{
		{Original: "Elijah", Replacement: "Alex"},
		{Original: "TechStartup Inc", Replacement: "NovoTech Solutions"},
		{Original: "$85,000", Replacement: "$90,000"},
	}
	for i, r := range replacements {
		if r.Original != expected[i].Original || r.Replacement != expected[i].Replacement {
			t.Errorf("replacement[%d]: got %+v, want %+v", i, r, expected[i])
		}
	}
}

func TestParseResponse_EmptyReplacements(t *testing.T) {
	content := `<think>

</think>

<tool_call>
{"name": "replace_entities", "arguments": {"replacements": []}}
</tool_call>`

	replacements, err := ParseResponse(content)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(replacements) != 0 {
		t.Fatalf("expected 0 replacements, got %d", len(replacements))
	}
}

func TestParseResponse_NoToolCall(t *testing.T) {
	// Model returned nothing useful (empty after think tags)
	content := `<think>

</think>

`
	replacements, err := ParseResponse(content)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if replacements != nil {
		t.Fatalf("expected nil replacements, got %+v", replacements)
	}
}

func TestParseResponse_MalformedJSON(t *testing.T) {
	content := `<tool_call>
not valid json
</tool_call>`

	_, err := ParseResponse(content)
	if err == nil {
		t.Fatal("expected error for malformed JSON")
	}
}

func TestParseResponse_NoThinkTags(t *testing.T) {
	// Some responses may not have think tags
	content := `<tool_call>
{"name": "replace_entities", "arguments": {"replacements": [{"original": "Jane", "replacement": "Emily"}]}}
</tool_call>`

	replacements, err := ParseResponse(content)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(replacements) != 1 {
		t.Fatalf("expected 1 replacement, got %d", len(replacements))
	}
	if replacements[0].Original != "Jane" {
		t.Errorf("got original %q, want %q", replacements[0].Original, "Jane")
	}
}
