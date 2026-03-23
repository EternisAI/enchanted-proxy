package anonymizer

import (
	"testing"
)

func TestApplyReplacements(t *testing.T) {
	text := "Hi, my son Elijah works at TechStartup Inc and makes $85,000 per year."
	replacements := []Replacement{
		{Original: "Elijah", Replacement: "Alex"},
		{Original: "TechStartup Inc", Replacement: "NovoTech Solutions"},
		{Original: "$85,000", Replacement: "$90,000"},
	}

	result := ApplyReplacements(text, replacements)
	expected := "Hi, my son Alex works at NovoTech Solutions and makes $90,000 per year."
	if result != expected {
		t.Errorf("got %q, want %q", result, expected)
	}
}

func TestApplyReplacements_LongestFirst(t *testing.T) {
	// "NovoTech" is a substring of "NovoTech Solutions" — must not partially match
	text := "She works at NovoTech Solutions and likes NovoTech."
	replacements := []Replacement{
		{Original: "NovoTech", Replacement: "AcmeCo"},
		{Original: "NovoTech Solutions", Replacement: "AcmeCo Industries"},
	}

	result := ApplyReplacements(text, replacements)
	expected := "She works at AcmeCo Industries and likes AcmeCo."
	if result != expected {
		t.Errorf("got %q, want %q", result, expected)
	}
}

func TestApplyReplacements_Empty(t *testing.T) {
	text := "No PII here."
	result := ApplyReplacements(text, nil)
	if result != text {
		t.Errorf("got %q, want %q", result, text)
	}
}

func TestApplyReplacements_MultipleOccurrences(t *testing.T) {
	text := "Elijah called Elijah's friend."
	replacements := []Replacement{
		{Original: "Elijah", Replacement: "Alex"},
	}

	result := ApplyReplacements(text, replacements)
	expected := "Alex called Alex's friend."
	if result != expected {
		t.Errorf("got %q, want %q", result, expected)
	}
}

func TestBuildPrompt(t *testing.T) {
	prompt := BuildPrompt("Hello, my name is Joel.")

	if !contains(prompt, "<|im_start|>system") {
		t.Error("prompt missing system start tag")
	}
	if !contains(prompt, "<|im_start|>user") {
		t.Error("prompt missing user start tag")
	}
	if !contains(prompt, "Hello, my name is Joel.") {
		t.Error("prompt missing user text")
	}
	if !contains(prompt, "/no_think") {
		t.Error("prompt missing /no_think directive")
	}
	if !contains(prompt, "<|im_start|>assistant") {
		t.Error("prompt missing assistant start tag")
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && searchString(s, substr)
}

func searchString(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
