package tasks

import (
	"context"
	"fmt"
	"net/http"

	"github.com/eternisai/enchanted-proxy/internal/auth"
	tasksdb "github.com/eternisai/enchanted-proxy/internal/storage/pg/queries/tasks"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/client"
)

type Handler struct {
	queries        tasksdb.Querier
	temporalClient client.Client
}

func NewHandler(queries tasksdb.Querier, temporalClient client.Client) *Handler {
	return &Handler{
		queries:        queries,
		temporalClient: temporalClient,
	}
}

func (h *Handler) CreateTask(c *gin.Context) {
	userID, ok := auth.GetUserUUID(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "User not authenticated"})
		return
	}

	var req CreateTaskRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request body"})
		return
	}

	ctx := context.Background()
	task, err := h.queries.CreateTask(ctx, tasksdb.CreateTaskParams{
		UserID:  userID,
		ChatID:  userID,
		Name:    req.Name,
		Content: req.Content,
		Cron:    req.Cron,
	})
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	taskId := uuid.New().String()
	id := fmt.Sprintf("task-%s", taskId)
	opts := client.ScheduleOptions{
		ID: id,
		Action: &client.ScheduleWorkflowAction{
			ID:        id,
			Workflow:  "ExecuteTaskWorkflow",
			Args:      []any{map[string]any{"name": req.Name}},
			TaskQueue: "task-queue",
		},
		Overlap: enums.SCHEDULE_OVERLAP_POLICY_SKIP,
		Spec: client.ScheduleSpec{
			CronExpressions: []string{req.Cron},
		},
	}

	scheduleHandle, err := h.temporalClient.ScheduleClient().Create(ctx, opts)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"temporal error": err.Error()})
		return
	}

	fmt.Println("scheduleHandle", scheduleHandle)

	response := TaskResponse{
		ID:        task.ID,
		Name:      task.Name,
		Content:   task.Content,
		Cron:      task.Cron,
		CreatedAt: task.CreatedAt,
		UpdatedAt: task.UpdatedAt,
	}

	c.JSON(http.StatusCreated, response)
}

func (h *Handler) GetTasks(c *gin.Context) {
	userID, ok := auth.GetUserUUID(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "User not authenticated"})
		return
	}

	ctx := context.Background()
	tasks, err := h.queries.GetTasksByChatID(ctx, userID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	taskResponses := make([]TaskResponse, 0, len(tasks))
	for _, task := range tasks {
		taskResponses = append(taskResponses, TaskResponse{
			ID:        task.ID,
			Name:      task.Name,
			Content:   task.Content,
			Cron:      task.Cron,
			CreatedAt: task.CreatedAt,
			UpdatedAt: task.UpdatedAt,
		})
	}

	response := TasksResponse{
		Tasks: taskResponses,
	}

	c.JSON(http.StatusOK, response)
}

func (h *Handler) DeleteTask(c *gin.Context) {
	userID, ok := auth.GetUserUUID(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "User not authenticated"})
		return
	}

	taskID := c.Param("id")
	if taskID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Task ID is required"})
		return
	}

	ctx := context.Background()
	err := h.queries.DeleteTaskByIDAndChatID(ctx, tasksdb.DeleteTaskByIDAndChatIDParams{
		ID:     taskID,
		ChatID: userID,
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "Task deleted successfully"})
}
