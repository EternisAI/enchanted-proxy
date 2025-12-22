package tools

import (
	"log/slog"
	"testing"
)

// NOTE: This file contains basic tests for the schedule_task tool.
// Full integration tests require a real task.Service with Temporal configuration.
// Those are tested in integration test suite.

func TestScheduledTasksTool_Name(t *testing.T) {
	// Test only checks tool name without needing a service
	tool := &ScheduledTasksTool{}

	if tool.Name() != "schedule_task" {
		t.Errorf("expected tool name 'schedule_task', got '%s'", tool.Name())
	}
}

func TestScheduledTasksTool_Definition(t *testing.T) {
	// Test only checks tool definition without needing a service
	tool := &ScheduledTasksTool{}

	def := tool.Definition()

	// Verify basic structure
	if def.Type != "function" {
		t.Errorf("expected type 'function', got '%s'", def.Type)
	}

	if def.Function.Name != "schedule_task" {
		t.Errorf("expected function name 'schedule_task', got '%s'", def.Function.Name)
	}

	if def.Function.Description == "" {
		t.Error("expected non-empty description")
	}

	// Check that parameters exist
	params, ok := def.Function.Parameters["properties"]
	if !ok {
		t.Fatal("parameters should have 'properties' field")
	}

	props, ok := params.(map[string]interface{})
	if !ok {
		t.Fatal("properties should be a map")
	}

	// Verify key parameters exist in definition
	requiredParams := []string{"action", "taskName", "taskText", "type", "time", "chatId", "taskId"}
	for _, param := range requiredParams {
		if _, exists := props[param]; !exists {
			t.Errorf("expected parameter '%s' to exist in definition", param)
		}
	}

	// Verify action parameter has enum
	actionParam, ok := props["action"].(map[string]interface{})
	if !ok {
		t.Fatal("action parameter should be a map")
	}

	enum, ok := actionParam["enum"]
	if !ok {
		t.Error("action parameter should have enum")
	}

	// Verify enum contains expected values
	enumValues, ok := enum.([]string)
	if !ok {
		t.Fatal("enum should be a string array")
	}

	expectedEnums := map[string]bool{"list": true, "create": true, "delete": true}
	for _, v := range enumValues {
		if !expectedEnums[v] {
			t.Errorf("unexpected enum value: %s", v)
		}
		delete(expectedEnums, v)
	}

	if len(expectedEnums) > 0 {
		t.Errorf("missing enum values: %v", expectedEnums)
	}
}

func TestParseArguments(t *testing.T) {
	// Test the helper function used by the tool
	args := `{"action":"list"}`
	var parsed ScheduledTasksArgs

	err := ParseArguments(args, &parsed)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if parsed.Action != "list" {
		t.Errorf("expected action 'list', got '%s'", parsed.Action)
	}
}

func TestParseArguments_Invalid(t *testing.T) {
	// Test invalid JSON
	args := `{invalid json}`
	var parsed ScheduledTasksArgs

	err := ParseArguments(args, &parsed)
	if err == nil {
		t.Error("expected error for invalid JSON, got nil")
	}
}

// Suppress unused import warning
var _ = slog.LevelInfo
