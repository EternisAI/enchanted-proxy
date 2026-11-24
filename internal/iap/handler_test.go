package iap

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/eternisai/enchanted-proxy/internal/auth"
	"github.com/eternisai/enchanted-proxy/internal/logger"
	"github.com/gin-gonic/gin"
	appstore "github.com/richzw/appstore"
)

// fakeParser accepts the tokens it knows, standing in for Apple's signature.
type fakeParser map[string]*appstore.JWSTransaction

func (f fakeParser) ParseTransaction(jws string) (*appstore.JWSTransaction, error) {
	tx, ok := f[jws]
	if !ok {
		return nil, errors.New("invalid signature")
	}
	c := *tx
	return &c, nil
}

func serveAttach(t *testing.T, s *Service, userID string, identity *auth.Identity, jws string) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	h := NewHandler(s, logger.New(logger.Config{Level: slog.LevelError + 4}))
	engine := gin.New()
	engine.Use(func(c *gin.Context) {
		c.Set(string(auth.UserIDKey), userID)
		if identity != nil {
			c.Request = c.Request.WithContext(auth.WithIdentity(c.Request.Context(), *identity))
		}
	})
	engine.POST("/attach", h.AttachAppStoreSubscription)
	body, _ := json.Marshal(map[string]string{"jwsTransactionInfo": jws})
	req := httptest.NewRequest(http.MethodPost, "/attach", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, req)
	return rec
}

// A purchase the policy refuses is answered with a sentence the app can show,
// and nothing is written: the service's queries are a nil interface that
// panics on any call.
func TestRefusedPurchasesWriteNothing(t *testing.T) {
	family := lifetimeTx()
	family.InAppOwnershipType = "FAMILY_SHARED"
	sandbox := lifetimeTx()
	sandbox.Environment = appstore.Sandbox
	s := &Service{parser: fakeParser{"family": family, "sandbox": sandbox}, policy: testPolicy(), now: func() time.Time { return testNow }}

	for _, jws := range []string{"family", "sandbox"} {
		rec := serveAttach(t, s, "u", identity("someone@example.org", true), jws)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("%s: status %d, want 403 (%s)", jws, rec.Code, rec.Body)
		}
		var body struct {
			Error  string `json:"error"`
			Reason string `json:"reason"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.Reason != "purchase_refused" || body.Error == "" {
			t.Fatalf("%s: body %s", jws, rec.Body)
		}
	}
	if rec := serveAttach(t, s, "u", nil, "forged"); rec.Code != http.StatusBadRequest {
		t.Fatalf("unverifiable token: status %d, want 400", rec.Code)
	}
}
