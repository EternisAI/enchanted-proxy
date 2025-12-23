package tools

import (
	"context"
	"fmt"
	"strings"

	"github.com/eternisai/enchanted-proxy/internal/logger"
	"github.com/eternisai/enchanted-proxy/internal/task"
)

// ScheduledTasksTool implements task scheduling and management using the task service.
type ScheduledTasksTool struct {
	taskService *task.Service
	logger      *logger.Logger
}

// NewScheduledTasksTool creates a new scheduled tasks tool.
func NewScheduledTasksTool(taskService *task.Service, logger *logger.Logger) *ScheduledTasksTool {
	return &ScheduledTasksTool{
		taskService: taskService,
		logger:      logger,
	}
}

// Name returns the tool name.
func (t *ScheduledTasksTool) Name() string {
	return "schedule_task"
}

// Definition returns the OpenAI-compatible function definition.
func (t *ScheduledTasksTool) Definition() ToolDefinition {
	return ToolDefinition{
		Type: "function",
		Function: FunctionDef{
			Name:        "schedule_task",
			Description: "Manage scheduled reminders, recurring tasks, and automated research/search operations. Use this tool when the user wants to: be reminded of something at a specific time, get updates on a topic (stock prices, news, weather, etc.), or have automated searches performed on a schedule. When executed, the server-side LLM will either send a simple notification OR perform a web search and then send results. Can list existing tasks, create new scheduled tasks, or delete tasks by ID.",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"action": map[string]interface{}{
						"type":        "string",
						"description": "Action to perform: 'list', 'create', or 'delete'",
						"enum":        []string{"list", "create", "delete"},
					},
					"taskName": map[string]interface{}{
						"type":        "string",
						"description": "Clean, concise task title suitable for notifications (required for 'create' action). Should be 2-4 words max, NO prefixes like 'Task:', 'Reminder:', etc. This will be shown directly as the notification title. Examples: 'Call Mom' (not 'Reminder: Call Mom'), 'Tech News' (not 'Task: Tech News'), 'AAPL Stock' (not 'Check AAPL stock')",
					},
					"taskText": map[string]interface{}{
						"type":        "string",
						"description": "CRITICAL: Complete instructions for the server-side LLM to execute this task (required for 'create' action). The server LLM will ONLY see this field and must decide whether to: (1) send a notification with specific content, OR (2) perform a web search and then send results. Be explicit about: what action to take, what to search for or notify about, any criteria/conditions, and desired output format. Example for stock: 'Search for the current stock price of AAPL (Apple Inc.) and send notification with: 1) current price, 2) change percentage, 3) market status'. Example for news: 'Search for the top 3 trending technology news articles from today and send notification with: 1) headline, 2) news source, 3) 1-2 sentence summary for each article.'",
					},
					"type": map[string]interface{}{
						"type":        "string",
						"description": "Task type: 'one_time' or 'recurring' (required for 'create' action)",
						"enum":        []string{"one_time", "recurring"},
					},
					"time": map[string]interface{}{
						"type":        "string",
						"description": "CRITICAL: Cron expression for task schedule in UTC (required for 'create' action). This is THE MOST IMPORTANT field - the server-side LLM cannot change the execution time, so you MUST interpret the user's time request accurately and convert it to cron format in UTC. Format: 'minute hour day month dayOfWeek'. Examples: '0 9 * * *' = 9:00 AM UTC daily, '30 14 * * 1' = 2:30 PM UTC every Monday, '0 18 * * 1-5' = 6:00 PM UTC weekdays only, '0 */2 * * *' = every 2 hours. User says 'every morning' → use '0 9 * * *'. User says 'every weekday at 3pm' → use '0 15 * * 1-5'. User says 'twice a day' → use '0 9,18 * * *' (9 AM and 6 PM). IMPORTANT: You must convert the user's local time to UTC before creating the cron expression.",
					},
					"chatId": map[string]interface{}{
						"type":        "string",
						"description": "Chat ID where the task was created and where results will be displayed. This is automatically detected from the conversation context - you should NOT ask the user for this value. Only provide this parameter if calling the API directly outside of a chat context.",
					},
					"taskId": map[string]interface{}{
						"type":        "string",
						"description": "ID of the task to delete (required for 'delete' action)",
					},
				},
				"required":             []string{"action"},
				"additionalProperties": false,
			},
		},
	}
}

// ScheduledTasksArgs represents the arguments for scheduled tasks operations.
type ScheduledTasksArgs struct {
	Action   string `json:"action"`
	TaskName string `json:"taskName,omitempty"`
	TaskText string `json:"taskText,omitempty"`
	Type     string `json:"type,omitempty"`
	Time     string `json:"time,omitempty"`
	ChatID   string `json:"chatId,omitempty"`
	TaskID   string `json:"taskId,omitempty"`
}

// Execute runs the scheduled tasks operation.
func (t *ScheduledTasksTool) Execute(ctx context.Context, args string) (string, error) {
	// Parse arguments
	var taskArgs ScheduledTasksArgs
	if err := ParseArguments(args, &taskArgs); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}

	// Extract user ID from context
	userID, ok := getUserIDFromContext(ctx)
	if !ok {
		return "", fmt.Errorf("user not authenticated")
	}

	t.logger.Info("executing scheduled tasks tool",
		"action", taskArgs.Action,
		"user_id", userID)

	// Route to appropriate action handler
	switch taskArgs.Action {
	case "list":
		return t.executeList(ctx, userID)
	case "create":
		return t.executeCreate(ctx, userID, &taskArgs)
	case "delete":
		return t.executeDelete(ctx, userID, taskArgs.TaskID)
	default:
		return "", fmt.Errorf("invalid action: %s (must be 'list', 'create', or 'delete')", taskArgs.Action)
	}
}

// executeList retrieves all tasks for the user.
func (t *ScheduledTasksTool) executeList(ctx context.Context, userID string) (string, error) {
	tasks, err := t.taskService.GetTasksByUserID(ctx, userID)
	if err != nil {
		return "", fmt.Errorf("failed to retrieve tasks: %w", err)
	}

	if len(tasks) == 0 {
		return "You have no scheduled tasks.", nil
	}

	// Format tasks as human-readable list
	var lines []string
	lines = append(lines, fmt.Sprintf("You have %d scheduled task(s):", len(tasks)))
	for i, task := range tasks {
		status := task.Status
		if status == "active" {
			status = "active"
		}
		lines = append(lines, fmt.Sprintf("%d. %s (ID: %s, Type: %s, Status: %s, Schedule: %s)",
			i+1, task.TaskName, task.TaskID, task.Type, status, task.Time))
	}

	return strings.Join(lines, "\n"), nil
}

// executeCreate creates a new scheduled task.
func (t *ScheduledTasksTool) executeCreate(ctx context.Context, userID string, args *ScheduledTasksArgs) (string, error) {
	// Get chatID from context if not provided in args
	chatID := args.ChatID
	if chatID == "" {
		chatID = getChatIDFromContext(ctx)
	}

	// Validate chatID is available (either from args or context)
	if chatID == "" {
		return "", fmt.Errorf("chatId is required for create action (should be auto-detected from context)")
	}
	if args.TaskName == "" {
		return "", fmt.Errorf("taskName is required for create action")
	}
	if args.TaskText == "" {
		return "", fmt.Errorf("taskText is required for create action")
	}
	if args.Type == "" {
		return "", fmt.Errorf("type is required for create action (must be 'recurring' or 'one_time')")
	}
	if args.Time == "" {
		return "", fmt.Errorf("time is required for create action")
	}

	// Validate task type
	if args.Type != "recurring" && args.Type != "one_time" {
		return "", fmt.Errorf("invalid type: %s (must be 'recurring' or 'one_time')", args.Type)
	}

	// Validate cron format (basic check - 5 fields)
	cronParts := strings.Fields(args.Time)
	if len(cronParts) != 5 {
		return "", fmt.Errorf("invalid cron format: expected 5 fields (minute hour day month dayOfWeek), got %d", len(cronParts))
	}

	t.logger.Info("creating scheduled task",
		"user_id", userID,
		"task_name", args.TaskName,
		"task_type", args.Type,
		"cron", args.Time)

	// Create task request (use chatID from context or args)
	req := &task.CreateTaskRequest{
		ChatID:   chatID,
		TaskName: args.TaskName,
		TaskText: args.TaskText,
		Type:     args.Type,
		Time:     args.Time,
	}

	// Call task service
	createdTask, err := t.taskService.CreateTask(ctx, userID, req)
	if err != nil {
		return "", fmt.Errorf("failed to create task: %w", err)
	}

	// Format success response
	taskType := "recurring"
	if args.Type == "one_time" {
		taskType = "one-time"
	}

	return fmt.Sprintf("Successfully created %s task: %s\nTask ID: %s\nSchedule: %s (UTC)\n\nThe task will execute according to the schedule and results will appear in your chat.",
		taskType, createdTask.TaskName, createdTask.TaskID, createdTask.Time), nil
}

// executeDelete deletes a task by ID.
func (t *ScheduledTasksTool) executeDelete(ctx context.Context, userID string, taskID string) (string, error) {
	// Validate task ID
	if taskID == "" {
		return "", fmt.Errorf("taskId is required for delete action")
	}

	t.logger.Info("deleting scheduled task",
		"user_id", userID,
		"task_id", taskID)

	// Call task service
	err := t.taskService.DeleteTask(ctx, userID, taskID)
	if err != nil {
		if strings.Contains(err.Error(), "not found") || strings.Contains(err.Error(), "unauthorized") {
			return "", fmt.Errorf("task not found or you don't have permission to delete it (ID: %s)", taskID)
		}
		return "", fmt.Errorf("failed to delete task: %w", err)
	}

	return fmt.Sprintf("Successfully deleted task (ID: %s)", taskID), nil
}

// getUserIDFromContext extracts the user ID from the context.
func getUserIDFromContext(ctx context.Context) (string, bool) {
	userID, ok := ctx.Value(logger.ContextKeyUserID).(string)
	if !ok || userID == "" {
		return "", false
	}
	return userID, true
}

// getChatIDFromContext extracts the chat ID from the context.
// Returns empty string if not found (caller should handle).
func getChatIDFromContext(ctx context.Context) string {
	chatID, ok := ctx.Value(logger.ContextKeyChatID).(string)
	if !ok || chatID == "" {
		return ""
	}
	return chatID
}
