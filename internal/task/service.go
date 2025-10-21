package task

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"time"

	"github.com/eternisai/enchanted-proxy/internal/logger"
	pgdb "github.com/eternisai/enchanted-proxy/internal/storage/pg/sqlc"
	"github.com/google/uuid"
	"go.temporal.io/sdk/client"
)

// Service handles task scheduling operations
type Service struct {
	temporalClient client.Client
	queries        *pgdb.Queries
	logger         *logger.Logger
	namespace      string
}

// NewService creates a new task service
func NewService(endpoint, namespace, apiKey string, queries *pgdb.Queries, logger *logger.Logger) (*Service, error) {
	log := logger.WithComponent("task-service")

	if endpoint == "" || namespace == "" || apiKey == "" {
		return nil, fmt.Errorf("temporal configuration is incomplete: endpoint=%q, namespace=%q, apiKey=%q",
			endpoint, namespace, "***")
	}

	// Create Temporal client options
	clientOptions := client.Options{
		HostPort:  endpoint,
		Namespace: namespace,
		ConnectionOptions: client.ConnectionOptions{
			TLS: &tls.Config{
				MinVersion: tls.VersionTLS12,
			},
		},
		// For Temporal Cloud, use API key authentication
		Credentials: client.NewAPIKeyStaticCredentials(apiKey),
	}

	// Connect to Temporal
	temporalClient, err := client.Dial(clientOptions)
	if err != nil {
		log.Error("failed to create temporal client",
			slog.String("error", err.Error()),
			slog.String("endpoint", endpoint),
			slog.String("namespace", namespace))
		return nil, fmt.Errorf("failed to create temporal client: %w", err)
	}

	log.Info("temporal client initialized",
		slog.String("endpoint", endpoint),
		slog.String("namespace", namespace))

	return &Service{
		temporalClient: temporalClient,
		queries:        queries,
		logger:         logger,
		namespace:      namespace,
	}, nil
}

// Close closes the Temporal client
func (s *Service) Close() {
	if s.temporalClient != nil {
		s.temporalClient.Close()
	}
}

// CreateTask creates a new scheduled task
func (s *Service) CreateTask(ctx context.Context, userID string, req *CreateTaskRequest) (*Task, error) {
	log := s.logger.WithContext(ctx).WithComponent("task-service")

	log.Info("CreateTask service called",
		slog.String("user_id", userID),
		slog.String("task_type", req.Type),
		slog.String("task_name", req.TaskName))

	// Validate task type
	log.Info("validating task type", slog.String("type", req.Type))
	if req.Type != string(TaskTypeRecurring) && req.Type != string(TaskTypeOneTime) {
		log.Error("invalid task type", slog.String("type", req.Type))
		return nil, fmt.Errorf("invalid task type: %s (must be 'recurring' or 'one_time')", req.Type)
	}

	// Validate cron format (both types use cron)
	log.Info("validating cron format", slog.String("time", req.Time))
	// Basic cron validation - Temporal will do more thorough validation
	if req.Time == "" {
		log.Error("time is empty")
		return nil, fmt.Errorf("time cannot be empty")
	}

	// Generate a unique task ID
	taskID := uuid.New().String()
	log.Info("generated task ID", slog.String("task_id", taskID))

	// Create task in database
	log.Info("creating task in database")
	dbTask, err := s.queries.CreateTask(ctx, pgdb.CreateTaskParams{
		TaskID:   taskID,
		UserID:   userID,
		ChatID:   req.ChatID,
		TaskName: req.TaskName,
		TaskText: req.TaskText,
		Type:     req.Type,
		Time:     req.Time,
		Status:   string(TaskStatusPending),
	})
	if err != nil {
		log.Error("failed to create task in database",
			slog.String("error", err.Error()),
			slog.String("user_id", userID))
		return nil, fmt.Errorf("failed to create task: %w", err)
	}
	log.Info("task created in database successfully", slog.String("task_id", taskID))

	// Create Temporal Schedule for the task
	// The workflow name should match what you register in your worker service
	workflowName := "ScheduledTaskWorkflow"
	log.Info("preparing to create temporal schedule", slog.String("workflow_name", workflowName))

	// Workflow input that will be passed to your worker service
	workflowInput := map[string]interface{}{
		"task_id":   taskID,
		"user_id":   userID,
		"chat_id":   req.ChatID,
		"task_name": req.TaskName,
		"task_text": req.TaskText,
		"type":      req.Type,
		"time":      req.Time,
	}

	// Create schedule options with properly configured spec
	scheduleSpec := client.ScheduleSpec{
		CronExpressions: []string{req.Time},
	}

	// For one-time tasks, we need to limit execution to just once
	// We'll set the schedule to end after 2 minutes from now to ensure it only fires once
	if req.Type == string(TaskTypeOneTime) {
		log.Info("configuring one-time schedule with cron", slog.String("cron_expression", req.Time))
		// Calculate when this cron would first fire, then set end time shortly after
		// This ensures the schedule only executes once and then automatically completes
		endTime := time.Now().Add(2 * time.Minute)
		scheduleSpec.EndAt = endTime
		log.Info("one-time schedule will end at", slog.Time("end_time", endTime))
	} else {
		log.Info("configuring recurring schedule with cron", slog.String("cron_expression", req.Time))
	}

	scheduleOptions := client.ScheduleOptions{
		ID:   taskID,
		Spec: scheduleSpec,
		Action: &client.ScheduleWorkflowAction{
			ID:        taskID + "-workflow",
			Workflow:  workflowName,
			Args:      []interface{}{workflowInput},
			TaskQueue: "task-queue",
		},
	}

	log.Info("creating temporal schedule",
		slog.String("schedule_id", taskID),
		slog.String("task_queue", "task-queue"))
	scheduleHandle, err := s.temporalClient.ScheduleClient().Create(ctx, scheduleOptions)
	if err != nil {
		log.Error("failed to create temporal schedule",
			slog.String("error", err.Error()),
			slog.String("task_id", taskID))
		// Clean up the database entry
		log.Info("cleaning up database entry due to schedule creation failure")
		_ = s.queries.DeleteTask(ctx, taskID)
		return nil, fmt.Errorf("failed to create schedule: %w", err)
	}

	log.Info("temporal schedule created successfully",
		slog.String("task_id", taskID),
		slog.String("task_type", req.Type),
		slog.String("schedule_id", scheduleHandle.GetID()))

	// Update task status to active
	log.Info("updating task status to active")
	err = s.queries.UpdateTaskStatus(ctx, pgdb.UpdateTaskStatusParams{
		TaskID: taskID,
		Status: string(TaskStatusActive),
	})
	if err != nil {
		log.Warn("failed to update task status to active",
			slog.String("error", err.Error()),
			slog.String("task_id", taskID))
	}

	task := &Task{
		TaskID:    dbTask.TaskID,
		UserID:    dbTask.UserID,
		ChatID:    dbTask.ChatID,
		TaskName:  dbTask.TaskName,
		TaskText:  dbTask.TaskText,
		Type:      dbTask.Type,
		Time:      dbTask.Time,
		Status:    string(TaskStatusActive),
		CreatedAt: dbTask.CreatedAt,
		UpdatedAt: dbTask.UpdatedAt,
	}

	log.Info("task creation completed successfully, returning task object")
	return task, nil
}

// GetTasksByUserID retrieves all tasks for a specific user
func (s *Service) GetTasksByUserID(ctx context.Context, userID string) ([]*Task, error) {
	log := s.logger.WithContext(ctx).WithComponent("task-service")

	dbTasks, err := s.queries.GetTasksByUserID(ctx, userID)
	if err != nil {
		log.Error("failed to get tasks from database",
			slog.String("error", err.Error()),
			slog.String("user_id", userID))
		return nil, fmt.Errorf("failed to get tasks: %w", err)
	}

	tasks := make([]*Task, 0, len(dbTasks))
	for _, dbTask := range dbTasks {
		tasks = append(tasks, &Task{
			TaskID:    dbTask.TaskID,
			UserID:    dbTask.UserID,
			ChatID:    dbTask.ChatID,
			TaskName:  dbTask.TaskName,
			TaskText:  dbTask.TaskText,
			Type:      dbTask.Type,
			Time:      dbTask.Time,
			Status:    dbTask.Status,
			CreatedAt: dbTask.CreatedAt,
			UpdatedAt: dbTask.UpdatedAt,
		})
	}

	return tasks, nil
}

// DeleteTask deletes a task by task ID
func (s *Service) DeleteTask(ctx context.Context, taskID string) error {
	log := s.logger.WithContext(ctx).WithComponent("task-service")

	// Delete the Temporal schedule
	scheduleHandle := s.temporalClient.ScheduleClient().GetHandle(ctx, taskID)
	err := scheduleHandle.Delete(ctx)
	if err != nil {
		log.Warn("failed to delete temporal schedule",
			slog.String("error", err.Error()),
			slog.String("task_id", taskID))
		// Continue with deletion even if schedule deletion fails
	}

	// Delete from database
	err = s.queries.DeleteTask(ctx, taskID)
	if err != nil {
		log.Error("failed to delete task from database",
			slog.String("error", err.Error()),
			slog.String("task_id", taskID))
		return fmt.Errorf("failed to delete task: %w", err)
	}

	log.Info("task deleted successfully", slog.String("task_id", taskID))
	return nil
}
