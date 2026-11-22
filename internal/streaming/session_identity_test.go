package streaming

import (
	"log/slog"
	"testing"

	"github.com/eternisai/enchanted-proxy/internal/auth"
	"github.com/eternisai/enchanted-proxy/internal/logger"
)

func TestToolContextCarriesIdentity(t *testing.T) {
	log := logger.New(logger.Config{Level: slog.LevelError})
	session := NewStreamSession("chat-123", "msg-456", newMockSSEStream([]string{"data: test"}), log)

	if _, ok := auth.IdentityFromContext(session.getContextWithUserID()); ok {
		t.Error("tool context has an identity before SetIdentity")
	}

	want := auth.Identity{UserID: "user-1", SignInProvider: auth.ProviderGoogle, EmailVerified: true}
	session.SetUserID("user-1")
	session.SetIdentity(want)

	got, ok := auth.IdentityFromContext(session.getContextWithUserID())
	if !ok || got != want {
		t.Errorf("tool context identity = %+v (present %v), want %+v", got, ok, want)
	}
}
