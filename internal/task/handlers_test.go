package task

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/eternisai/enchanted-proxy/internal/auth"
	"github.com/eternisai/enchanted-proxy/internal/logger"
	"github.com/gin-gonic/gin"
)

// A service with no database or Temporal client: any call past validation would panic.
func newValidationOnlyService() *Service {
	return &Service{logger: logger.New(logger.Config{Level: slog.LevelError})}
}

func TestCreateTaskRefusesMalformedChatID(t *testing.T) {
	s := newValidationOnlyService()
	for _, chatID := range []string{"../x", "a/b", "..%2f..%2fx", ""} {
		_, err := s.CreateTask(context.Background(), "test-user", &CreateTaskRequest{
			ChatID: chatID, TaskName: "n", TaskText: "t", Type: string(TaskTypeRecurring), Time: "0 9 * * *",
		})
		if !errors.Is(err, ErrInvalidChatID) {
			t.Errorf("chat_id %q: err = %v, want ErrInvalidChatID", chatID, err)
		}
	}
}

func TestCreateTaskHandlerReturns400ForMalformedChatID(t *testing.T) {
	gin.SetMode(gin.TestMode)
	s := newValidationOnlyService()
	h := NewHandler(s, s.logger)
	engine := gin.New()
	engine.Use(func(c *gin.Context) { c.Set(string(auth.UserIDKey), "test-user") })
	engine.POST("/api/v1/tasks", h.CreateTask)

	body := `{"chat_id":"../x","task_name":"n","task_text":"t","type":"recurring","time":"0 9 * * *"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/tasks", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body %s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "Invalid chat_id") {
		t.Errorf("body %s lacks a readable error", w.Body.String())
	}
}
