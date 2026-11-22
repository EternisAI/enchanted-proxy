package invitecode

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/eternisai/enchanted-proxy/internal/auth"
	pgdb "github.com/eternisai/enchanted-proxy/internal/storage/pg/sqlc"
	"github.com/gin-gonic/gin"
)

// inviteQuerier knows no invite codes and counts whitelist entries per user.
// Any other query, such as creating a code, panics through the nil interface.
type inviteQuerier struct {
	pgdb.Querier
	whitelisted map[string]bool
	counted     []string
}

func (q *inviteQuerier) GetInviteCodeByCodeHash(context.Context, string) (pgdb.InviteCode, error) {
	return pgdb.InviteCode{}, sql.ErrNoRows
}

func (q *inviteQuerier) CountInviteCodesByRedeemedBy(_ context.Context, userID *string) (int64, error) {
	q.counted = append(q.counted, *userID)
	if q.whitelisted[*userID] {
		return 1, nil
	}
	return 0, nil
}

func newInviteRouter(q *inviteQuerier) *gin.Engine {
	gin.SetMode(gin.TestMode)
	h := NewHandler(NewService(q))
	engine := gin.New()
	engine.Use(func(c *gin.Context) { c.Set(string(auth.UserIDKey), "caller") })
	engine.GET("/api/v1/invites/:userID/whitelist", h.CheckUserWhitelist)
	engine.POST("/api/v1/invites/:code/redeem", h.RedeemInviteCode)
	return engine
}

func serve(engine *gin.Engine, method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)
	return w
}

func TestRedeemSuffixCodeIsNotSpecial(t *testing.T) {
	q := &inviteQuerier{}
	w := serve(newInviteRouter(q), http.MethodPost, "/api/v1/invites/anything-eternis/redeem", `{"access_token":"x"}`)
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 (body %s)", w.Code, w.Body.String())
	}
}

func TestWhitelistOnlyAnswersForCaller(t *testing.T) {
	q := &inviteQuerier{whitelisted: map[string]bool{"caller": true, "someone-else": true}}
	engine := newInviteRouter(q)

	w := serve(engine, http.MethodGet, "/api/v1/invites/caller/whitelist", "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"whitelisted":true`) {
		t.Errorf("own ID: status %d, body %s", w.Code, w.Body.String())
	}

	w = serve(engine, http.MethodGet, "/api/v1/invites/someone-else/whitelist", "")
	if w.Code != http.StatusForbidden {
		t.Errorf("other ID: status = %d, want 403", w.Code)
	}
	if len(q.counted) != 1 || q.counted[0] != "caller" {
		t.Errorf("whitelist queries = %v, want only the caller", q.counted)
	}
}
