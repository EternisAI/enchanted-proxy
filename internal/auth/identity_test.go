package auth

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	fbauth "firebase.google.com/go/v4/auth"
	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v4"
)

// Firebase ID token payloads as each sign-in type produces them.
var (
	anonymousToken = &fbauth.Token{
		Subject:  "anon-uid",
		Firebase: fbauth.FirebaseInfo{SignInProvider: "anonymous"},
		Claims:   map[string]interface{}{"firebase": map[string]interface{}{"sign_in_provider": "anonymous"}},
	}
	unverifiedEmailToken = &fbauth.Token{
		Subject:  "email-uid",
		Firebase: fbauth.FirebaseInfo{SignInProvider: "password"},
		Claims:   map[string]interface{}{"email": "someone@example.com", "email_verified": false},
	}
	appleToken = &fbauth.Token{
		Subject:  "apple-uid",
		Firebase: fbauth.FirebaseInfo{SignInProvider: "apple.com"},
		Claims:   map[string]interface{}{"email": "relay@privaterelay.appleid.com", "email_verified": true},
	}
)

type wantIdentity struct {
	userID        string
	anonymous     bool
	appleOrGoogle bool
	verifiedEmail string
}

func checkIdentity(t *testing.T, name string, got Identity, want wantIdentity) {
	t.Helper()
	if got.UserID != want.userID {
		t.Errorf("%s: UserID = %q, want %q", name, got.UserID, want.userID)
	}
	if got.IsAnonymous() != want.anonymous {
		t.Errorf("%s: IsAnonymous() = %v, want %v", name, got.IsAnonymous(), want.anonymous)
	}
	if got.SignedInWith(ProviderApple, ProviderGoogle) != want.appleOrGoogle {
		t.Errorf("%s: SignedInWith(apple, google) = %v, want %v", name, !want.appleOrGoogle, want.appleOrGoogle)
	}
	email, ok := got.VerifiedEmail()
	if email != want.verifiedEmail || ok != (want.verifiedEmail != "") {
		t.Errorf("%s: VerifiedEmail() = (%q, %v), want %q", name, email, ok, want.verifiedEmail)
	}
}

func TestIdentityFromFirebaseToken(t *testing.T) {
	checkIdentity(t, "anonymous", identityFromFirebaseToken(anonymousToken),
		wantIdentity{userID: "anon-uid", anonymous: true})
	checkIdentity(t, "unverified email", identityFromFirebaseToken(unverifiedEmailToken),
		wantIdentity{userID: "email-uid"})
	checkIdentity(t, "apple", identityFromFirebaseToken(appleToken),
		wantIdentity{userID: "apple-uid", appleOrGoogle: true, verifiedEmail: "relay@privaterelay.appleid.com"})

	if got := identityFromFirebaseToken(unverifiedEmailToken); got.Email != "someone@example.com" || got.SignInProvider != ProviderPassword {
		t.Errorf("unverified email: got provider %q, email set %v", got.SignInProvider, got.Email != "")
	}
}

func TestSignedInWithEmptyProvider(t *testing.T) {
	if (Identity{UserID: "u"}).SignedInWith("") {
		t.Error("an identity with no provider must not match the empty provider")
	}
}

func signedTestToken(t *testing.T, claims jwt.MapClaims) string {
	t.Helper()
	s, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte("test-key"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestJWTValidatorExtractIdentity(t *testing.T) {
	v, err := NewTokenValidator("") // development mode: parses without verifying
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		claims jwt.MapClaims
		want   wantIdentity
	}{
		{"anonymous", jwt.MapClaims{"sub": "anon-uid", "firebase": map[string]interface{}{"sign_in_provider": "anonymous"}},
			wantIdentity{userID: "anon-uid", anonymous: true}},
		{"unverified email", jwt.MapClaims{"sub": "email-uid", "email": "someone@example.com", "email_verified": false,
			"firebase": map[string]interface{}{"sign_in_provider": "password"}},
			wantIdentity{userID: "email-uid"}},
		{"apple", jwt.MapClaims{"sub": "apple-uid", "email": "relay@privaterelay.appleid.com", "email_verified": true,
			"firebase": map[string]interface{}{"sign_in_provider": "apple.com"}},
			wantIdentity{userID: "apple-uid", appleOrGoogle: true, verifiedEmail: "relay@privaterelay.appleid.com"}},
		{"no provider claim", jwt.MapClaims{"user_id": "plain-uid"},
			wantIdentity{userID: "plain-uid"}},
	}
	for _, tt := range tests {
		got, err := v.ExtractIdentity(signedTestToken(t, tt.claims))
		if err != nil {
			t.Fatalf("%s: %v", tt.name, err)
		}
		checkIdentity(t, tt.name, got, tt.want)
	}

	if _, err := v.ExtractIdentity(signedTestToken(t, jwt.MapClaims{"firebase": map[string]interface{}{"sign_in_provider": "anonymous"}})); err == nil {
		t.Error("a token without sub, user_id or email must be refused")
	}
}

type fixedValidator struct{ id Identity }

func (f fixedValidator) ExtractIdentity(string) (Identity, error) { return f.id, nil }

func TestRequireAuthExposesIdentity(t *testing.T) {
	gin.SetMode(gin.TestMode)
	want := identityFromFirebaseToken(appleToken)
	mw, _ := NewFirebaseAuthMiddleware(fixedValidator{id: want})

	var fromGin, fromCtx Identity
	var userID string
	engine := gin.New()
	engine.Use(mw.RequireAuth())
	engine.GET("/", func(c *gin.Context) {
		fromGin, _ = GetIdentity(c)
		fromCtx, _ = IdentityFromContext(c.Request.Context())
		userID, _ = GetUserID(c)
		c.Status(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer token")
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	if fromGin != want || fromCtx != want {
		t.Errorf("identity from gin = %+v, from context = %+v, want %+v", fromGin, fromCtx, want)
	}
	if userID != want.UserID {
		t.Errorf("GetUserID = %q, want %q", userID, want.UserID)
	}
}

func TestIdentityLogValueOmitsEmail(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, nil))
	log.Info("caller", slog.Any("identity", identityFromFirebaseToken(appleToken)))
	out := buf.String()
	if strings.Contains(out, "privaterelay") || strings.Contains(out, "@") {
		t.Errorf("log line contains the email: %s", out)
	}
	if !strings.Contains(out, `"sign_in_provider":"apple.com"`) || !strings.Contains(out, `"email_verified":true`) {
		t.Errorf("log line lacks the non-personal facts: %s", out)
	}
}
