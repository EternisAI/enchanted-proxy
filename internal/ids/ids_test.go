package ids

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestValid(t *testing.T) {
	tests := []struct {
		id   string
		want bool
	}{
		{"0d6f6b3e-8a54-4c1e-9a1f-2f4b6c7d8e9f", true},
		{"0D6F6B3E-8A54-4C1E-9A1F-2F4B6C7D8E9F", true},
		{"temp-0d6f6b3e-8a54-4c1e-9a1f-2f4b6c7d8e9f", true},
		{"abc_DEF-123", true},
		{"a", true},
		{strings.Repeat("a", MaxLength), true},
		{"", false},
		{strings.Repeat("a", MaxLength+1), false},
		{"../x", false},
		{"a/b", false},
		{"..%2f..%2fx", false},
		{"..", false},
		{"a.b", false},
		{"a b", false},
		{"a\nb", false},
		{"a\\b", false},
		{"a?b=c", false},
		{"a#b", false},
		{"ä", false},
	}
	for _, tt := range tests {
		if got := Valid(tt.id); got != tt.want {
			t.Errorf("Valid(%q) = %v, want %v", tt.id, got, tt.want)
		}
	}
}

func TestValidOptional(t *testing.T) {
	if !ValidOptional("") {
		t.Error("ValidOptional(\"\") = false, want true")
	}
	if ValidOptional("a/b") {
		t.Error("ValidOptional(\"a/b\") = true, want false")
	}
}

func TestMiddleware(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Use(Middleware())
	ok := func(c *gin.Context) { c.Status(http.StatusOK) }
	engine.POST("/chat/completions", ok)
	engine.POST("/api/v1/chats/:chatId/messages/:messageId/stop", ok)

	const uuid = "0d6f6b3e-8a54-4c1e-9a1f-2f4b6c7d8e9f"
	tests := []struct {
		name    string
		path    string
		headers map[string]string
		want    int
	}{
		{"no ids", "/chat/completions", nil, http.StatusOK},
		{"valid headers", "/chat/completions", map[string]string{"X-Chat-ID": uuid, "X-Message-ID": uuid, "X-User-Message-ID": uuid}, http.StatusOK},
		{"traversal in chat header", "/chat/completions", map[string]string{"X-Chat-ID": "../x"}, http.StatusBadRequest},
		{"slash in message header", "/chat/completions", map[string]string{"X-Message-ID": "a/b"}, http.StatusBadRequest},
		{"encoded traversal in user message header", "/chat/completions", map[string]string{"X-User-Message-ID": "..%2f..%2fx"}, http.StatusBadRequest},
		{"valid route params", "/api/v1/chats/" + uuid + "/messages/" + uuid + "/stop", nil, http.StatusOK},
		{"dot-dot chat param", "/api/v1/chats/../messages/" + uuid + "/stop", nil, http.StatusBadRequest},
		{"percent in message param", "/api/v1/chats/" + uuid + "/messages/a%25b/stop", nil, http.StatusBadRequest},
		{"dot in message param", "/api/v1/chats/" + uuid + "/messages/a.b/stop", nil, http.StatusBadRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/", nil)
			req.URL.Path = tt.path // set directly so "../" segments are not cleaned away
			for k, v := range tt.headers {
				req.Header.Set(k, v)
			}
			w := httptest.NewRecorder()
			engine.ServeHTTP(w, req)
			if w.Code != tt.want {
				t.Errorf("status = %d, want %d (body %s)", w.Code, tt.want, w.Body.String())
			}
		})
	}
}
