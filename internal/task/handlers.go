package task

import (
	"log/slog"
	"net/http"

	"github.com/eternisai/enchanted-proxy/internal/auth"
	"github.com/eternisai/enchanted-proxy/internal/errors"
	"github.com/eternisai/enchanted-proxy/internal/logger"
	"github.com/gin-gonic/gin"
)

// Handler handles HTTP requests for task operations.
type Handler struct {
	service *Service
	logger  *logger.Logger
}

// NewHandler creates a new task handler.
func NewHandler(service *Service, logger *logger.Logger) *Handler {
	return &Handler{
		service: service,
		logger:  logger,
	}
}

// CreateTask handles POST /api/v1/tasks
// Creates a new scheduled task.
func (h *Handler) CreateTask(c *gin.Context) {
	log := h.logger.WithContext(c.Request.Context()).WithComponent("task-handler")

	log.Info("CreateTask handler called")

	userID, ok := auth.GetUserID(c)
	if !ok {
		log.Error("user not authenticated")
		errors.Unauthorized(c, "unauthorized", nil)
		return
	}

	log.Info("user authenticated", slog.String("user_id", userID))

	// Parse request body
	var req CreateTaskRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		log.Error("failed to bind request", slog.String("error", err.Error()))
		errors.BadRequest(c, "invalid request body", map[string]interface{}{"details": err.Error()})
		return
	}

	log.Info("request body parsed",
		slog.String("chat_id", req.ChatID),
		slog.String("task_name", req.TaskName),
		slog.String("task_type", req.Type),
		slog.String("cron_expression", req.Time))

	// Create the task
	log.Info("calling service.CreateTask")
	task, err := h.service.CreateTask(c.Request.Context(), userID, &req)
	if err != nil {
		log.Error("failed to create task",
			slog.String("error", err.Error()),
			slog.String("user_id", userID))
		errors.Internal(c, "failed to create task", map[string]interface{}{"details": err.Error()})
		return
	}

	log.Info("task created successfully",
		slog.String("task_id", task.TaskID),
		slog.String("user_id", userID),
		slog.String("task_type", task.Type))

	c.JSON(http.StatusCreated, CreateTaskResponse{Task: task})
	log.Info("response sent to client")
}

// GetTasks handles GET /api/v1/tasks
// Returns all tasks for the authenticated user.
func (h *Handler) GetTasks(c *gin.Context) {
	log := h.logger.WithContext(c.Request.Context()).WithComponent("task-handler")

	userID, ok := auth.GetUserID(c)
	if !ok {
		log.Error("user not authenticated")
		errors.Unauthorized(c, "unauthorized", nil)
		return
	}

	// Get tasks for the user
	tasks, err := h.service.GetTasksByUserID(c.Request.Context(), userID)
	if err != nil {
		log.Error("failed to get tasks",
			slog.String("error", err.Error()),
			slog.String("user_id", userID))
		errors.Internal(c, "failed to get tasks", map[string]interface{}{"details": err.Error()})
		return
	}

	log.Info("tasks retrieved successfully",
		slog.String("user_id", userID),
		slog.Int("count", len(tasks)))

	c.JSON(http.StatusOK, GetTasksResponse{Tasks: tasks})
}

// DeleteTask handles DELETE /api/v1/tasks/:taskId
// Deletes a specific task.
func (h *Handler) DeleteTask(c *gin.Context) {
	log := h.logger.WithContext(c.Request.Context()).WithComponent("task-handler")

	userID, ok := auth.GetUserID(c)
	if !ok {
		log.Error("user not authenticated")
		errors.Unauthorized(c, "unauthorized", nil)
		return
	}

	// Get task ID from URL parameter
	taskID := c.Param("taskId")
	if taskID == "" {
		log.Error("task_id is empty")
		errors.BadRequest(c, "task_id is required", nil)
		return
	}

	// Delete the task with ownership verification
	err := h.service.DeleteTask(c.Request.Context(), userID, taskID)
	if err != nil {
		// Check if task not found or unauthorized
		if err.Error() == "task not found or unauthorized" {
			log.Warn("task not found or unauthorized",
				slog.String("task_id", taskID),
				slog.String("user_id", userID))
			errors.NotFound(c, "task not found", nil)
			return
		}

		log.Error("failed to delete task",
			slog.String("error", err.Error()),
			slog.String("task_id", taskID),
			slog.String("user_id", userID))
		errors.Internal(c, "failed to delete task", map[string]interface{}{"details": err.Error()})
		return
	}

	log.Info("task deleted successfully",
		slog.String("task_id", taskID),
		slog.String("user_id", userID))

	c.JSON(http.StatusOK, DeleteTaskResponse{
		Success: true,
		Message: "task deleted successfully",
	})
}
