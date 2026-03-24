package streaming

import (
	"testing"
)

func TestNewDeanonymizer_EmptyInput(t *testing.T) {
	if NewDeanonymizer("") != nil {
		t.Fatal("expected nil for empty string")
	}
	if NewDeanonymizer("[]") != nil {
		t.Fatal("expected nil for empty array")
	}
	if NewDeanonymizer("invalid json") != nil {
		t.Fatal("expected nil for invalid JSON")
	}
}

func TestDeanonymizer_ReplaceInText(t *testing.T) {
	d := NewDeanonymizer(`[{"original":"123 Apple St","replacement":"123 Pear St"},{"original":"John","replacement":"Mike"}]`)
	if d == nil {
		t.Fatal("expected non-nil deanonymizer")
	}

	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"exact match", "I live at 123 Pear St", "I live at 123 Apple St"},
		{"case insensitive", "i live at 123 pear st", "i live at 123 Apple St"},
		{"multiple replacements", "Mike lives at 123 Pear St", "John lives at 123 Apple St"},
		{"no match", "nothing to replace here", "nothing to replace here"},
		{"empty string", "", ""},
		{"partial — no match", "123 Pear", "123 Pear"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := d.ReplaceInText(tt.input)
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestDeanonymizer_LongestMatchFirst(t *testing.T) {
	// "123 Pear Street" should match before "Pear" alone
	d := NewDeanonymizer(`[{"original":"Apple","replacement":"Pear"},{"original":"123 Apple Street","replacement":"123 Pear Street"}]`)
	if d == nil {
		t.Fatal("expected non-nil deanonymizer")
	}

	got := d.ReplaceInText("I live at 123 Pear Street")
	want := "I live at 123 Apple Street"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestDeanonymizer_ReplaceInSSELine(t *testing.T) {
	d := NewDeanonymizer(`[{"original":"John Smith","replacement":"Mike Jones"}]`)
	if d == nil {
		t.Fatal("expected non-nil deanonymizer")
	}

	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			"content delta with match",
			`data: {"choices":[{"delta":{"content":"Hello Mike Jones"}}]}`,
			`data: {"choices":[{"delta":{"content":"Hello John Smith"}}]}`,
		},
		{
			"no content field",
			`data: {"choices":[{"delta":{"role":"assistant"}}]}`,
			`data: {"choices":[{"delta":{"role":"assistant"}}]}`,
		},
		{
			"DONE line",
			`data: [DONE]`,
			`data: [DONE]`,
		},
		{
			"non-data line",
			`event: message`,
			`event: message`,
		},
		{
			"empty content",
			`data: {"choices":[{"delta":{"content":""}}]}`,
			`data: {"choices":[{"delta":{"content":""}}]}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := d.ReplaceInSSELine(tt.input)
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}
