package auth

import (
	"context"
	"errors"
)

type contextKey struct{}

var (
	ErrNoContext     = errors.New("no security context present")
	ErrNotAuthorized = errors.New("not authorized")
)

// SecurityContext represents the security context for the current request.
type SecurityContext interface {
	GetUserID() string
	HasUserID(string) bool
}

// JWTSecurityContext implements SecurityContext with JWT authentication.
type JWTSecurityContext struct {
	UserID string
}

func (b *JWTSecurityContext) GetUserID() string {
	return b.UserID
}

func (b *JWTSecurityContext) HasUserID(id string) bool {
	return b.UserID == id
}

// To adds a security context to a context.
func To(ctx context.Context, sc SecurityContext) context.Context {
	return context.WithValue(ctx, contextKey{}, sc)
}

// For gets the security context from a context.
func For(ctx context.Context) SecurityContext {
	if sc, ok := ctx.Value(contextKey{}).(SecurityContext); ok {
		return sc
	}
	panic(ErrNoContext)
}

// ForErr gets the security context from a context, returning an error if not present.
func ForErr(ctx context.Context) (SecurityContext, error) {
	if sc, ok := ctx.Value(contextKey{}).(SecurityContext); ok {
		return sc, nil
	}
	return nil, ErrNoContext
}
