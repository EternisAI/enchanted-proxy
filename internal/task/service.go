package task

import (
	"context"
	"crypto/tls"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/eternisai/enchanted-proxy/internal/ids"
	"github.com/eternisai/enchanted-proxy/internal/logger"
	pgdb "github.com/eternisai/enchanted-proxy/internal/storage/pg/sqlc"
	"github.com/google/uuid"
	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"
)

// scheduleCatchupWindow is how late a schedule may still take an action it
// missed while Temporal was unavailable.
const scheduleCatchupWindow = 5 * time.Minute

// oneTimeWindow is the shortest gap a one-time schedule may have between runs.
// Its Temporal schedule is pinned to a single run, so this is not what keeps it
// from running twice: an expression with runs minutes apart names several
// times, not one, and is refused so that one is chosen.
const oneTimeWindow = 5 * time.Minute

// After a task is admitted, the steps that schedule it or clean up after a
// failure run on contexts that keep the request's values but not its
// cancellation: a client that disconnects must not interrupt them halfway.
// Each step is bounded on its own.
const (
	scheduleSetupTimeout = 30 * time.Second
	cleanupTimeout       = 10 * time.Second
)

// detached returns a context with ctx's values, without its cancellation, and
// with its own deadline.
func detached(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), timeout)
}

// ErrInvalidChatID is returned when a task's chat ID is malformed.
var ErrInvalidChatID = errors.New("invalid chat_id")

// ErrTaskNotFound is returned when the task does not exist or belongs to
// another account.
var ErrTaskNotFound = errors.New("task not found or unauthorized")

// Service handles task scheduling operations.
type Service struct {
	temporalClient client.Client
	queries        pgdb.Querier
	logger         *logger.Logger
	namespace      string
	tierLookup     TierLookup
	groups         GroupLookup
	admission      admissionTx
	clock          func() time.Time // time.Now outside tests
}

func (s *Service) currentTime() time.Time {
	if s.clock != nil {
		return s.clock()
	}
	return time.Now()
}

// NewService creates a new task service. tierLookup and groups supply the plan
// limits applied when a task is created; db serializes the admission of new
// tasks per quota group.
func NewService(endpoint, namespace, apiKey string, db *sql.DB, queries pgdb.Querier, tierLookup TierLookup, groups GroupLookup, logger *logger.Logger) (*Service, error) {
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
		tierLookup:     tierLookup,
		groups:         groups,
		admission:      lockedAdmission(db),
		clock:          time.Now,
	}, nil
}

// Close closes the Temporal client.
func (s *Service) Close() {
	if s.temporalClient != nil {
		s.temporalClient.Close()
	}
}

// releaseTask removes a task that was admitted but could not be scheduled,
// together with its recorded creation, so the attempt does not count toward
// the owner's limits. Call it only once no schedule can run. It does not stop
// when the client disconnects. A failure here leaves the rows in place, which
// counts the attempt rather than losing it.
func (s *Service) releaseTask(ctx context.Context, taskID, userID string) {
	log := s.logger.WithContext(ctx).WithComponent("task-service")
	ctx, cancel := detached(ctx, cleanupTimeout)
	defer cancel()
	if _, err := s.queries.DeleteTask(ctx, pgdb.DeleteTaskParams{TaskID: taskID, UserID: userID}); err != nil {
		log.Error("failed to delete unscheduled task", slog.String("error", err.Error()), slog.String("task_id", taskID))
		return
	}
	if err := s.queries.DeleteTaskCreation(ctx, taskID); err != nil {
		log.Error("failed to release task creation", slog.String("error", err.Error()), slog.String("task_id", taskID))
	}
}

// scheduleExists reports whether Temporal holds a schedule. It is asked when
// creating the schedule returned an error, because a failed call does not
// prove that the schedule was not created. An error means the answer is
// unknown.
func (s *Service) scheduleExists(ctx context.Context, scheduleID string) (bool, error) {
	ctx, cancel := detached(ctx, cleanupTimeout)
	defer cancel()
	_, err := s.temporalClient.ScheduleClient().GetHandle(ctx, scheduleID).Describe(ctx)
	var notFound *serviceerror.NotFound
	switch {
	case err == nil:
		return true, nil
	case errors.As(err, &notFound):
		return false, nil
	default:
		return false, err
	}
}

// isNotFound reports whether Temporal answered that the schedule does not exist.
func isNotFound(err error) bool {
	var notFound *serviceerror.NotFound
	return errors.As(err, &notFound)
}

// rejectedOutright reports whether Temporal refused a request in a way that
// rules out the schedule being created later. After any other error, such as
// a deadline or an unavailable service, the request may still be processed,
// so "not found" does not settle the outcome.
func rejectedOutright(err error) bool {
	var invalid *serviceerror.InvalidArgument
	var denied *serviceerror.PermissionDenied
	var noNamespace *serviceerror.NamespaceNotFound
	return errors.As(err, &invalid) || errors.As(err, &denied) || errors.As(err, &noNamespace)
}

// CreateTask creates a new scheduled task.
func (s *Service) CreateTask(ctx context.Context, userID string, req *CreateTaskRequest) (*Task, error) {
	log := s.logger.WithContext(ctx).WithComponent("task-service")

	log.Info("CreateTask service called",
		slog.String("user_id", userID),
		slog.String("task_type", req.Type),
		slog.String("task_name", req.TaskName))

	if !ids.Valid(req.ChatID) {
		log.Warn("refusing task with malformed chat_id")
		return nil, ErrInvalidChatID
	}

	// Validate task type
	log.Info("validating task type", slog.String("type", req.Type))
	if req.Type != string(TaskTypeRecurring) && req.Type != string(TaskTypeOneTime) {
		log.Error("invalid task type", slog.String("type", req.Type))
		return nil, fmt.Errorf("invalid task type: %s (must be 'recurring' or 'one_time')", req.Type)
	}

	// Validate cron format (both types use cron)
	log.Info("validating cron format", slog.String("cron_expression", req.Time))
	// Basic cron validation - Temporal will do more thorough validation
	if req.Time == "" {
		log.Error("time is empty")
		return nil, fmt.Errorf("time cannot be empty")
	}

	// Generate a unique task ID
	taskID := uuid.New().String()
	log.Info("generated task ID", slog.String("task_id", taskID))

	// Check the plan limits, insert the task as pending and record its creation,
	// under the quota group's admission lock.
	started := s.currentTime()
	dbTask, err := s.admitTask(ctx, userID, taskID, req, started)
	if err != nil {
		var limitErr *LimitError
		var validationErr *ValidationError
		if errors.As(err, &limitErr) || errors.As(err, &validationErr) {
			log.Warn("refusing task", slog.String("error", err.Error()))
		} else {
			log.Error("failed to admit task", slog.String("error", err.Error()), slog.String("user_id", userID))
		}
		return nil, err
	}
	log.Info("task created in database successfully", slog.String("task_id", taskID))

	// Create Temporal Schedule for the task
	// The workflow name should match what you register in your worker service
	workflowName := scheduledTaskWorkflow
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
	if req.Type == string(TaskTypeOneTime) {
		log.Info("configuring one-time schedule with cron", slog.String("cron_expression", req.Time))
		schedule, err := parseStrictSchedule(req.Time)
		if err != nil {
			log.Error("invalid cron expression for one-time task",
				slog.String("error", err.Error()),
				slog.String("cron_expression", req.Time))
			s.releaseTask(ctx, taskID, userID)
			return nil, fmt.Errorf("invalid cron expression: %w", err)
		}
		// The run is the first after the task row's creation time, which is
		// also what pending accounting reads, so the two always agree.
		nextRun := schedule.Next(dbTask.CreatedAt)
		if !nextRun.After(s.currentTime()) {
			// The run passed while the task was being admitted. Temporal does not
			// backfill a new schedule, so it would never run. No schedule exists
			// yet, so the task can be released.
			log.Warn("one-time run passed during admission", slog.String("task_id", taskID))
			s.releaseTask(ctx, taskID, userID)
			return nil, &ValidationError{Message: "The scheduled time passed while the task was being created. Choose a later time."}
		}
		// Start and end at that run (an inclusive interval), so the schedule
		// fires exactly once.
		scheduleSpec.StartAt = nextRun
		scheduleSpec.EndAt = nextRun
		log.Info("one-time schedule configured", slog.Time("expected_run", nextRun))
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
		// A run still going when the next is due is skipped, and runs missed while
		// Temporal was unavailable are not replayed after a few minutes.
		Overlap:       enumspb.SCHEDULE_OVERLAP_POLICY_SKIP,
		CatchupWindow: scheduleCatchupWindow,
		// The owner, so sweeps of the schedules do not need the database.
		Memo: map[string]interface{}{"user_id": userID},
	}

	log.Info("creating temporal schedule",
		slog.String("schedule_id", taskID),
		slog.String("task_queue", "task-queue"))
	schedules := s.temporalClient.ScheduleClient()
	setupCtx, cancelSetup := detached(ctx, scheduleSetupTimeout)
	defer cancelSetup()
	scheduleHandle, err := schedules.Create(setupCtx, scheduleOptions)
	if err != nil {
		log.Error("failed to create temporal schedule",
			slog.String("error", err.Error()),
			slog.String("task_id", taskID))
		// The schedule may have been created even so, or may still be. Its task
		// is released only when Temporal refused the request outright and holds
		// no schedule. Otherwise it stays pending and counted; the owner can
		// delete it, and the reconciler removes pending tasks that have no
		// schedule.
		exists, checkErr := s.scheduleExists(ctx, taskID)
		switch {
		case checkErr != nil:
			log.Error("could not confirm whether the schedule exists; the task stays counted",
				slog.String("error", checkErr.Error()),
				slog.String("task_id", taskID))
			return nil, fmt.Errorf("failed to create schedule: %w", err)
		case !exists && rejectedOutright(err):
			log.Info("cleaning up database entry due to schedule creation failure")
			s.releaseTask(ctx, taskID, userID)
			return nil, fmt.Errorf("failed to create schedule: %w", err)
		case !exists:
			log.Warn("schedule not found, but the request may still be processed; the task stays counted",
				slog.String("task_id", taskID))
			return nil, fmt.Errorf("failed to create schedule: %w", err)
		}
		log.Warn("the schedule was created despite the error; continuing", slog.String("task_id", taskID))
		scheduleHandle = schedules.GetHandle(ctx, taskID)
	}

	log.Info("temporal schedule created successfully",
		slog.String("task_id", taskID),
		slog.String("task_type", req.Type),
		slog.String("schedule_id", scheduleHandle.GetID()))

	// Update task status to active
	log.Info("updating task status to active")
	activateCtx, cancelActivate := detached(ctx, cleanupTimeout)
	defer cancelActivate()
	err = s.queries.UpdateTaskStatus(activateCtx, pgdb.UpdateTaskStatusParams{
		TaskID: taskID,
		Status: string(TaskStatusActive),
	})
	if err != nil {
		log.Error("failed to update task status to active",
			slog.String("error", err.Error()),
			slog.String("task_id", taskID))
		// Status update failed - try to clean up the schedule
		deleteCtx, cancelDelete := detached(ctx, cleanupTimeout)
		defer cancelDelete()
		if delErr := scheduleHandle.Delete(deleteCtx); delErr != nil {
			// The schedule may still run, so its rows stay and keep it counted.
			log.Error("failed to delete schedule during cleanup",
				slog.String("error", delErr.Error()),
				slog.String("task_id", taskID))
			return nil, fmt.Errorf("failed to update task status: %w", err)
		}
		s.releaseTask(ctx, taskID, userID)
		return nil, fmt.Errorf("failed to update task status: %w", err)
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

// GetTasksByUserID retrieves all tasks for a specific user.
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
		tasks = append(tasks, taskFromRow(dbTask))
	}

	return tasks, nil
}

// getOwnedTask reads a task row of the caller's.
func getOwnedTask(ctx context.Context, q pgdb.Querier, userID, taskID string) (pgdb.Task, error) {
	row, err := q.GetTaskForUser(ctx, pgdb.GetTaskForUserParams{TaskID: taskID, UserID: userID})
	if errors.Is(err, sql.ErrNoRows) {
		return pgdb.Task{}, ErrTaskNotFound
	}
	if err != nil {
		return pgdb.Task{}, fmt.Errorf("failed to read task: %w", err)
	}
	return row, nil
}

// deleteSchedule removes a task's schedule. A schedule that does not exist
// counts as removed. After a failed call the schedule may be gone even so,
// which a lookup settles; while it may still exist, the error is returned.
func (s *Service) deleteSchedule(ctx context.Context, scheduleID string) error {
	err := s.temporalClient.ScheduleClient().GetHandle(ctx, scheduleID).Delete(ctx)
	if err == nil || isNotFound(err) {
		return nil
	}
	if exists, checkErr := s.scheduleExists(ctx, scheduleID); checkErr == nil && !exists {
		return nil
	}
	return err
}

// DeleteTask deletes one of the caller's tasks. The schedule goes first: the
// row is removed only once the schedule is gone, so a schedule cannot keep
// running without a row that counts it. When the schedule may still exist,
// the row stays and an error is returned.
func (s *Service) DeleteTask(ctx context.Context, userID, taskID string) error {
	log := s.logger.WithContext(ctx).WithComponent("task-service")

	if _, err := getOwnedTask(ctx, s.queries, userID, taskID); err != nil {
		if errors.Is(err, ErrTaskNotFound) {
			log.Warn("task not found or unauthorized",
				slog.String("task_id", taskID),
				slog.String("user_id", userID))
		}
		return err
	}

	// The steps do not stop when the client disconnects.
	opCtx, cancel := detached(ctx, scheduleSetupTimeout)
	defer cancel()
	if err := s.deleteSchedule(opCtx, taskID); err != nil {
		log.Error("failed to delete temporal schedule; the task is kept",
			slog.String("error", err.Error()),
			slog.String("task_id", taskID))
		return fmt.Errorf("failed to delete the task's schedule: %w", err)
	}

	// A row already gone was deleted by a concurrent request; the outcome is the same.
	if _, err := s.queries.DeleteTask(opCtx, pgdb.DeleteTaskParams{TaskID: taskID, UserID: userID}); err != nil {
		log.Error("failed to delete task from database after its schedule",
			slog.String("error", err.Error()),
			slog.String("task_id", taskID),
			slog.String("user_id", userID))
		return fmt.Errorf("failed to delete task: %w", err)
	}

	log.Info("task deleted successfully",
		slog.String("task_id", taskID),
		slog.String("user_id", userID))
	return nil
}
