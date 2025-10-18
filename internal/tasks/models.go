package tasks

import "time"

type Task struct {
	ID        string    `json:"id"`
	ChatID    string    `json:"chat_id"`
	Name      string    `json:"name"`
	Content   string    `json:"content"`
	Cron      string    `json:"cron"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type CreateTaskRequest struct {
	Name    string `json:"name" binding:"required"`
	Content string `json:"content" binding:"required"`
	Cron    string `json:"cron" binding:"required"`
}

type TaskResponse struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Content   string    `json:"content"`
	Cron      string    `json:"cron"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type TasksResponse struct {
	Tasks []TaskResponse `json:"tasks"`
}
