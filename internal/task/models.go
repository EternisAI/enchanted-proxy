package task

import "time"

// Task represents a scheduled task in the system.
type Task struct {
	TaskID    string    `json:"task_id" db:"task_id"`
	UserID    string    `json:"user_id" db:"user_id"`
	ChatID    string    `json:"chat_id" db:"chat_id"`
	TaskName  string    `json:"task_name" db:"task_name"`
	TaskText  string    `json:"task_text" db:"task_text"`
	Type      string    `json:"type" db:"type"` // "recurring" or "one_time"
	Time      string    `json:"time" db:"time"` // cron format for both types
	Status    string    `json:"status" db:"status"`
	CreatedAt time.Time `json:"created_at" db:"created_at"`
	UpdatedAt time.Time `json:"updated_at" db:"updated_at"`
}

// TaskType represents the type of task scheduling.
type TaskType string

const (
	TaskTypeRecurring TaskType = "recurring"
	TaskTypeOneTime   TaskType = "one_time"
)

// TaskStatus represents the status of a task.
type TaskStatus string

const (
	TaskStatusPending   TaskStatus = "pending"
	TaskStatusActive    TaskStatus = "active"
	TaskStatusPaused    TaskStatus = "paused"
	TaskStatusCompleted TaskStatus = "completed"
	TaskStatusFailed    TaskStatus = "failed"
)

// CreateTaskRequest represents the request to create a new task.
type CreateTaskRequest struct {
	ChatID   string `json:"chat_id" binding:"required"`
	TaskName string `json:"task_name" binding:"required"`
	TaskText string `json:"task_text" binding:"required"`
	Type     string `json:"type" binding:"required"` // "recurring" or "one_time"
	Time     string `json:"time" binding:"required"` // cron format for both types (e.g., "0 9 * * *" for daily at 9am, "30 14 20 8 *" for one-time on Aug 20 at 14:30)
}

// CreateTaskResponse represents the response when creating a task.
type CreateTaskResponse struct {
	Task *Task `json:"task"`
}

// GetTasksResponse represents the response when getting tasks.
type GetTasksResponse struct {
	Tasks []*Task `json:"tasks"`
}

// DeleteTaskResponse represents the response when deleting a task.
type DeleteTaskResponse struct {
	Success bool   `json:"success"`
	Message string `json:"message"`
}
