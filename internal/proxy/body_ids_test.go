package proxy

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/eternisai/enchanted-proxy/internal/logger"
	"github.com/gin-gonic/gin"
)

// Malformed chatId and messageId in the request body are refused before any
// dependency is used, so the handler can run with none configured.
func TestProxyHandlerRefusesMalformedBodyIDs(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.POST("/chat/completions", ProxyHandler(logger.New(logger.Config{Level: slog.LevelError}), nil, nil, nil, nil, nil, nil, nil, nil, nil))

	for _, body := range []string{
		`{"model":"m","chatId":"../x"}`,
		`{"model":"m","chatId":"a/b"}`,
		`{"model":"m","chatId":"0d6f6b3e-8a54-4c1e-9a1f-2f4b6c7d8e9f","messageId":"..%2f..%2fx"}`,
	} {
		req := httptest.NewRequest(http.MethodPost, "/chat/completions", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		engine.ServeHTTP(w, req)
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", body, w.Code)
		}
	}
}
