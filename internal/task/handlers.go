package task

import (
	"log/slog"
	"net/http"

	"github.com/eternisai/enchanted-proxy/internal/auth"
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

	// Check if user is authenticated (using GetUserUUID for auth check)
	_, ok := auth.GetUserUUID(c)
	if !ok {
		log.Error("user not authenticated")
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}

	// Get Firebase UID from context - this will be used as the user_id
	userID, ok := auth.GetFirebaseUID(c)
	if !ok {
		log.Error("firebase_uid not found in context")
		c.JSON(http.StatusUnauthorized, gin.H{"error": "firebase uid not found"})
		return
	}

	log.Info("user authenticated", slog.String("user_id", userID))

	// Parse request body
	var req CreateTaskRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		log.Error("failed to bind request", slog.String("error", err.Error()))
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body", "details": err.Error()})
		return
	}

	log.Info("request body parsed",
		slog.String("chat_id", req.ChatID),
		slog.String("task_name", req.TaskName),
		slog.String("task_type", req.Type),
		slog.String("time", req.Time))

	// Create the task
	log.Info("calling service.CreateTask")
	task, err := h.service.CreateTask(c.Request.Context(), userID, &req)
	if err != nil {
		log.Error("failed to create task",
			slog.String("error", err.Error()),
			slog.String("user_id", userID))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create task", "details": err.Error()})
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

	// Check if user is authenticated
	_, ok := auth.GetUserUUID(c)
	if !ok {
		log.Error("user not authenticated")
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}

	// Get Firebase UID from context - this will be used as the user_id
	userID, ok := auth.GetFirebaseUID(c)
	if !ok {
		log.Error("firebase_uid not found in context")
		c.JSON(http.StatusUnauthorized, gin.H{"error": "firebase uid not found"})
		return
	}

	// Get tasks for the user
	tasks, err := h.service.GetTasksByUserID(c.Request.Context(), userID)
	if err != nil {
		log.Error("failed to get tasks",
			slog.String("error", err.Error()),
			slog.String("user_id", userID))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get tasks", "details": err.Error()})
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

	// Check if user is authenticated
	_, ok := auth.GetUserUUID(c)
	if !ok {
		log.Error("user not authenticated")
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}

	// Get Firebase UID from context - this will be used as the user_id
	userID, ok := auth.GetFirebaseUID(c)
	if !ok {
		log.Error("firebase_uid not found in context")
		c.JSON(http.StatusUnauthorized, gin.H{"error": "firebase uid not found"})
		return
	}

	// Get task ID from URL parameter
	taskID := c.Param("taskId")
	if taskID == "" {
		log.Error("task_id is empty")
		c.JSON(http.StatusBadRequest, gin.H{"error": "task_id is required"})
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
			c.JSON(http.StatusNotFound, gin.H{"error": "task not found"})
			return
		}

		log.Error("failed to delete task",
			slog.String("error", err.Error()),
			slog.String("task_id", taskID),
			slog.String("user_id", userID))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete task", "details": err.Error()})
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
