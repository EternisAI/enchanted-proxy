package auth

import (
	"errors"

	"github.com/golang-jwt/jwt/v4"
)

var (
	ErrInvalidToken = errors.New("invalid token")
	ErrExpiredToken = errors.New("token has expired")
	ErrNoJWKS       = errors.New("no JWKS URL provided")
)

// StandardClaims represents the standard claims in a JWT token.
type StandardClaims struct {
	// Standard JWT claims
	Sub    string `json:"sub"`
	UserId string `json:"user_id"`
	Email  string `json:"email"`
	jwt.RegisteredClaims
}

// UserInfo contains extracted user information from a token.
type UserInfo struct {
	UserID         string
	SignInProvider string // e.g., "anonymous", "google.com", "password"
}

// IsAnonymous returns true if the user authenticated anonymously.
func (u UserInfo) IsAnonymous() bool {
	return u.SignInProvider == "anonymous"
}

type TokenValidator interface {
	ExtractUserInfo(tokenString string) (UserInfo, error)
}
