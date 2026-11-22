package auth

import (
	"errors"
	"fmt"

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
	Sub           string `json:"sub"`
	UserId        string `json:"user_id"`
	Email         string `json:"email"`
	EmailVerified bool   `json:"email_verified"`
	// Firebase ID tokens carry the sign-in provider under "firebase".
	Firebase struct {
		SignInProvider string `json:"sign_in_provider"`
	} `json:"firebase"`
	jwt.RegisteredClaims
}

// identity builds the caller's Identity from validated claims. The user ID
// is sub, then user_id, then email, as before.
func (c *StandardClaims) identity() (Identity, error) {
	userID := c.Sub
	if userID == "" {
		userID = c.UserId
	}
	if userID == "" {
		userID = c.Email
	}
	if userID == "" {
		return Identity{}, fmt.Errorf("%w: no sub, user_id, or email found in token claims", ErrInvalidToken)
	}
	return Identity{
		UserID:         userID,
		SignInProvider: c.Firebase.SignInProvider,
		Email:          c.Email,
		EmailVerified:  c.EmailVerified,
	}, nil
}

// TokenValidator validates an ID token and returns the verified facts about its holder.
type TokenValidator interface {
	ExtractIdentity(tokenString string) (Identity, error)
}
