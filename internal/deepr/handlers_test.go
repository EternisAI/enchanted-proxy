package deepr

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/eternisai/enchanted-proxy/internal/auth"
	"github.com/eternisai/enchanted-proxy/internal/logger"
	pgdb "github.com/eternisai/enchanted-proxy/internal/storage/pg/sqlc"
	"github.com/gin-gonic/gin"
)

// untouchedQuerier panics on any query, proving a refused request never reached the database.
type untouchedQuerier struct{ pgdb.Querier }

func testLogger() *logger.Logger {
	return logger.New(logger.Config{Level: slog.LevelError})
}

func withUser(c *gin.Context) {
	c.Set(string(auth.UserIDKey), "test-user")
}

func newRefusalRouter() *gin.Engine {
	gin.SetMode(gin.TestMode)
	log := testLogger()
	q := untouchedQuerier{}
	engine := gin.New()
	engine.Use(withUser)
	engine.POST("/start", StartDeepResearchHandler(log, nil, nil, nil, nil, q, true, nil, nil, nil))
	engine.POST("/clarify", ClarifyDeepResearchHandler(log, nil, nil, nil, nil, q, true, nil))
	engine.GET("/ws", DeepResearchHandler(log, nil, nil, nil, nil, q, true, nil))
	return engine
}

func TestStartAndClarifyRefuseMalformedIDs(t *testing.T) {
	engine := newRefusalRouter()
	for _, path := range []string{"/start", "/clarify"} {
		for _, body := range []string{
			`{"query":"q","response":"r","chat_id":"../x"}`,
			`{"query":"q","response":"r","chat_id":"a/b"}`,
			`{"query":"q","response":"r","chat_id":"..%2f..%2fx"}`,
			`{"query":"q","response":"r","chat_id":"0d6f6b3e-8a54-4c1e-9a1f-2f4b6c7d8e9f","user_message_id":"../m"}`,
		} {
			req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			engine.ServeHTTP(w, req)
			if w.Code != http.StatusBadRequest {
				t.Errorf("%s %s: status = %d, want 400", path, body, w.Code)
			}
			if !strings.Contains(w.Body.String(), "Invalid") {
				t.Errorf("%s %s: body %s lacks a readable error", path, body, w.Body.String())
			}
		}
	}
}

func TestWebSocketRefusesMalformedChatID(t *testing.T) {
	engine := newRefusalRouter()
	for _, raw := range []string{"..%2f..%2fx", "a%2Fb", "..", "a.b"} {
		req := httptest.NewRequest(http.MethodGet, "/ws?chat_id="+raw, nil)
		w := httptest.NewRecorder()
		engine.ServeHTTP(w, req)
		if w.Code != http.StatusBadRequest {
			t.Errorf("chat_id=%s: status = %d, want 400", raw, w.Code)
		}
	}
}

func TestBackendSessionURLEscapesIDs(t *testing.T) {
	u := backendSessionURL("ws", "backend:3031", "user1", "a/b?c")
	got := u.String()
	want := "ws://backend:3031/deep_research/user1/a%2Fb%3Fc/"
	if got != want {
		t.Errorf("URL = %q, want %q", got, want)
	}
	parsed, err := url.Parse(got)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.RawQuery != "" {
		t.Errorf("chat ID leaked into the query string: %q", parsed.RawQuery)
	}

	plain := backendSessionURL("ws", "backend:3031", "user1", "0d6f6b3e-8a54-4c1e-9a1f-2f4b6c7d8e9f")
	if s := plain.String(); s != "ws://backend:3031/deep_research/user1/0d6f6b3e-8a54-4c1e-9a1f-2f4b6c7d8e9f/" {
		t.Errorf("URL for a UUID = %q", s)
	}
}
