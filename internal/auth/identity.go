package auth

import (
	"context"
	"log/slog"
	"slices"

	"github.com/gin-gonic/gin"
)

// Sign-in providers as Firebase reports them in the ID token's
// firebase.sign_in_provider claim.
const (
	ProviderAnonymous = "anonymous"
	ProviderPassword  = "password"
	ProviderApple     = "apple.com"
	ProviderGoogle    = "google.com"
	ProviderCustom    = "custom"
)

// Identity holds the verified facts about the caller that access rules may
// depend on. It comes from the validated ID token, never from the request
// body or headers.
type Identity struct {
	UserID string
	// SignInProvider is how the account signed in: one of the Provider
	// constants, another Firebase provider ID, or empty when the token does
	// not say.
	SignInProvider string
	// Email is the account's email address, if it has one. It is personal
	// data: do not log it.
	Email string
	// EmailVerified is the token's email_verified claim. Email-and-password
	// sign-up does not verify addresses, so an unverified email proves nothing.
	EmailVerified bool
}

// IsAnonymous reports whether the account is a Firebase anonymous account.
func (i Identity) IsAnonymous() bool {
	return i.SignInProvider == ProviderAnonymous
}

// SignedInWith reports whether the account signed in with one of providers.
func (i Identity) SignedInWith(providers ...string) bool {
	return i.SignInProvider != "" && slices.Contains(providers, i.SignInProvider)
}

// VerifiedEmail returns the account's email only when it is marked verified.
func (i Identity) VerifiedEmail() (string, bool) {
	if i.Email == "" || !i.EmailVerified {
		return "", false
	}
	return i.Email, true
}

// LogValue keeps the email address out of logs when an Identity is logged.
func (i Identity) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("user_id", i.UserID),
		slog.String("sign_in_provider", i.SignInProvider),
		slog.Bool("has_email", i.Email != ""),
		slog.Bool("email_verified", i.EmailVerified),
	)
}

type identityContextKey struct{}

// identityGinKey is the gin context key under which RequireAuth stores the Identity.
const identityGinKey = "auth_identity"

// WithIdentity returns a copy of ctx carrying id.
func WithIdentity(ctx context.Context, id Identity) context.Context {
	return context.WithValue(ctx, identityContextKey{}, id)
}

// IdentityFromContext returns the Identity stored by WithIdentity, if any.
// Code that runs outside the gin handler, such as chat tools, uses this.
func IdentityFromContext(ctx context.Context) (Identity, bool) {
	id, ok := ctx.Value(identityContextKey{}).(Identity)
	return id, ok
}

// GetIdentity returns the caller's Identity set by RequireAuth.
func GetIdentity(c *gin.Context) (Identity, bool) {
	if v, ok := c.Get(identityGinKey); ok {
		if id, ok := v.(Identity); ok {
			return id, true
		}
	}
	return IdentityFromContext(c.Request.Context())
}
