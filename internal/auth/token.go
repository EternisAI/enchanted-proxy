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

// TokenValidator validates JWT tokens and extracts user ID.
type TokenValidator interface {
	ValidateToken(tokenString string) (string, error)
}

// FirebaseUIDProvider is an optional interface that token validators can implement
// to provide the Firebase UID in addition to the user identifier.
type FirebaseUIDProvider interface {
	GetFirebaseUID(tokenString string) (string, error)
}
