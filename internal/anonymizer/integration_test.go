//go:build integration

package anonymizer

import (
	"context"
	"os"
	"testing"
	"time"
)

// These tests hit the real anonymizer CVM endpoint.
// Run with: ANONYMIZER_TEST_URL=... ANONYMIZER_TEST_KEY=... go test -tags=integration -v ./internal/anonymizer/
//
// Required env vars:
//   ANONYMIZER_TEST_URL - base URL (e.g. https://b775ab37.igw.ghostagent.org)
//   ANONYMIZER_TEST_KEY - API key

func getTestConfig(t *testing.T) ClientConfig {
	baseURL := os.Getenv("ANONYMIZER_TEST_URL")
	apiKey := os.Getenv("ANONYMIZER_TEST_KEY")
	if baseURL == "" || apiKey == "" {
		t.Skip("ANONYMIZER_TEST_URL and ANONYMIZER_TEST_KEY must be set")
	}
	return ClientConfig{
		BaseURL: baseURL,
		APIKey:  apiKey,
		Timeout: 30 * time.Second,
	}
}

func TestIntegration_AnonymizeWithPII(t *testing.T) {
	client := NewClient(getTestConfig(t))
	svc := NewService(client)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	result, err := svc.Anonymize(ctx, "Hi, my son Elijah works at TechStartup Inc and makes $85,000 per year.")
	if err != nil {
		t.Fatalf("Anonymize failed: %v", err)
	}

	if len(result.Replacements) == 0 {
		t.Fatal("expected non-empty replacements for message with PII")
	}

	// The anonymized text should not contain the original PII
	for _, r := range result.Replacements {
		if r.Original == r.Replacement {
			t.Errorf("replacement is same as original: %q", r.Original)
		}
	}

	t.Logf("Original: Hi, my son Elijah works at TechStartup Inc and makes $85,000 per year.")
	t.Logf("Anonymized: %s", result.Text)
	t.Logf("Replacements: %+v", result.Replacements)
}

func TestIntegration_AnonymizeNoPII(t *testing.T) {
	client := NewClient(getTestConfig(t))
	svc := NewService(client)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	result, err := svc.Anonymize(ctx, "What is the capital of France?")
	if err != nil {
		t.Fatalf("Anonymize failed: %v", err)
	}

	if len(result.Replacements) != 0 {
		t.Logf("WARNING: got replacements for non-PII message: %+v", result.Replacements)
		// Not a hard failure — model might occasionally flag things
	}

	t.Logf("Text: %s", result.Text)
	t.Logf("Replacements: %+v", result.Replacements)
}

func TestIntegration_ClientCallDirect(t *testing.T) {
	client := NewClient(getTestConfig(t))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	content, err := client.Call(ctx, "My email is john.doe@example.com and I live at 123 Oak Street, Smallville.")
	if err != nil {
		t.Fatalf("Call failed: %v", err)
	}

	t.Logf("Raw response content: %s", content)

	replacements, err := ParseResponse(content)
	if err != nil {
		t.Fatalf("ParseResponse failed: %v", err)
	}

	if len(replacements) == 0 {
		t.Fatal("expected replacements for message with email and address")
	}

	t.Logf("Replacements: %+v", replacements)
}
