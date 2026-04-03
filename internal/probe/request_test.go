package probe

import (
	"testing"

	"github.com/eternisai/enchanted-proxy/internal/routing"
)

func TestIsOpenAIReasoningModel(t *testing.T) {
	tests := []struct {
		model string
		want  bool
	}{
		// Positive: known reasoning models.
		{"o1-preview", true},
		{"o1-mini", true},
		{"o3-mini", true},
		{"o4-mini", true},
		{"O1-Preview", true},   // case-insensitive
		{"O3-MINI", true},      // all caps
		{"o4-mini-2025", true}, // dated variant

		// Negative: must not match without delimiter.
		{"o1", false},
		{"o3", false},
		{"o4", false},
		{"ollama-something", false},
		{"o1custom", false},
		{"o3special", false},
		{"o4audit-log", false},

		// Negative: other model families.
		{"gpt-4o", false},
		{"claude-3-opus", false},
		{"", false},
	}

	for _, tt := range tests {
		t.Run(tt.model, func(t *testing.T) {
			if got := isOpenAIReasoningModel(tt.model); got != tt.want {
				t.Errorf("isOpenAIReasoningModel(%q) = %v, want %v", tt.model, got, tt.want)
			}
		})
	}
}

func TestBuildResponsesProbeRequestBody(t *testing.T) {
	tests := []struct {
		name            string
		endpoint        *routing.ProviderConfig
		probe           *routing.ProbeConfig
		wantReasoning   string // expected reasoning.effort value, "" if key should be absent
	}{
		{
			name:     "basic responses probe",
			endpoint: &routing.ProviderConfig{Model: "gpt-5.4-pro"},
			probe: &routing.ProbeConfig{
				Prompt:    "Say OK",
				MaxTokens: 100,
				Thinking:  false,
			},
			wantReasoning: "low",
		},
		{
			name:     "thinking enabled — no reasoning suppression",
			endpoint: &routing.ProviderConfig{Model: "gpt-5.4-pro"},
			probe: &routing.ProbeConfig{
				Prompt:    "Say OK",
				MaxTokens: 100,
				Thinking:  true,
			},
			wantReasoning: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := buildResponsesProbeRequestBody(tt.endpoint, tt.probe)

			// Verify model.
			if body["model"] != tt.endpoint.Model {
				t.Errorf("model = %v, want %v", body["model"], tt.endpoint.Model)
			}

			// Verify input (not messages).
			if _, exists := body["messages"]; exists {
				t.Error("responses probe should use 'input', not 'messages'")
			}
			input, ok := body["input"].([]map[string]string)
			if !ok || len(input) != 1 {
				t.Fatalf("input has unexpected shape: %v", body["input"])
			}
			if input[0]["content"] != tt.probe.Prompt {
				t.Errorf("input content = %q, want %q", input[0]["content"], tt.probe.Prompt)
			}

			// Verify max_output_tokens (not max_tokens).
			if _, exists := body["max_tokens"]; exists {
				t.Error("responses probe should use 'max_output_tokens', not 'max_tokens'")
			}
			if body["max_output_tokens"] != tt.probe.MaxTokens {
				t.Errorf("max_output_tokens = %v, want %v", body["max_output_tokens"], tt.probe.MaxTokens)
			}

			// Verify store=false.
			if body["store"] != false {
				t.Errorf("store = %v, want false", body["store"])
			}

			// Verify no stream or background.
			if _, exists := body["stream"]; exists {
				t.Error("responses probe should not set 'stream'")
			}
			if _, exists := body["background"]; exists {
				t.Error("responses probe should not set 'background'")
			}

			// Verify no temperature.
			if _, exists := body["temperature"]; exists {
				t.Error("responses probe should not set 'temperature'")
			}

			// Verify reasoning.
			reasoning, exists := body["reasoning"]
			if tt.wantReasoning != "" {
				if !exists {
					t.Fatal("expected reasoning key to be present")
				}
				r, ok := reasoning.(map[string]any)
				if !ok {
					t.Fatalf("reasoning has unexpected type: %T", reasoning)
				}
				if r["effort"] != tt.wantReasoning {
					t.Errorf("reasoning.effort = %v, want %v", r["effort"], tt.wantReasoning)
				}
			} else if exists {
				t.Errorf("unexpected reasoning key: %v", reasoning)
			}
		})
	}
}

func TestBuildProbeRequestBody(t *testing.T) {
	expected := "pong"
	tests := []struct {
		name             string
		endpoint         *routing.ProviderConfig
		probe            *routing.ProbeConfig
		wantReasoning    bool   // expect reasoning_effort key
		wantReasoningVal string // expected value
	}{
		{
			name:     "standard model, thinking disabled",
			endpoint: &routing.ProviderConfig{Model: "gpt-4o"},
			probe: &routing.ProbeConfig{
				Prompt:           "say ping",
				ExpectedResponse: &expected,
				MaxTokens:        10,
				Temperature:      0,
				Thinking:         false,
			},
			wantReasoning: false,
		},
		{
			name:     "reasoning model, thinking disabled — sets reasoning_effort",
			endpoint: &routing.ProviderConfig{Model: "o3-mini"},
			probe: &routing.ProbeConfig{
				Prompt:           "say ping",
				ExpectedResponse: &expected,
				MaxTokens:        10,
				Temperature:      0,
				Thinking:         false,
			},
			wantReasoning:    true,
			wantReasoningVal: "low",
		},
		{
			name:     "reasoning model, thinking enabled — no reasoning_effort",
			endpoint: &routing.ProviderConfig{Model: "o3-mini"},
			probe: &routing.ProbeConfig{
				Prompt:           "say ping",
				ExpectedResponse: &expected,
				MaxTokens:        10,
				Temperature:      0,
				Thinking:         true,
			},
			wantReasoning: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := buildProbeRequestBody(tt.endpoint, tt.probe)

			// Verify required fields.
			if body["model"] != tt.endpoint.Model {
				t.Errorf("model = %v, want %v", body["model"], tt.endpoint.Model)
			}
			if body["stream"] != false {
				t.Errorf("stream = %v, want false", body["stream"])
			}
			if body["max_tokens"] != tt.probe.MaxTokens {
				t.Errorf("max_tokens = %v, want %v", body["max_tokens"], tt.probe.MaxTokens)
			}

			// Verify messages.
			msgs, ok := body["messages"].([]map[string]string)
			if !ok || len(msgs) != 1 {
				t.Fatalf("messages has unexpected shape: %v", body["messages"])
			}
			if msgs[0]["content"] != tt.probe.Prompt {
				t.Errorf("message content = %q, want %q", msgs[0]["content"], tt.probe.Prompt)
			}

			// Verify reasoning_effort.
			val, exists := body["reasoning_effort"]
			if tt.wantReasoning {
				if !exists {
					t.Fatal("expected reasoning_effort key to be present")
				}
				if val != tt.wantReasoningVal {
					t.Errorf("reasoning_effort = %v, want %v", val, tt.wantReasoningVal)
				}
			} else if exists {
				t.Errorf("unexpected reasoning_effort key: %v", val)
			}
		})
	}
}
