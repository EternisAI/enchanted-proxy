package auth

import (
	"context"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v4"
	"github.com/lestrrat-go/jwx/jwk"
)

// JWTTokenValidator is a concrete implementation of TokenValidator for JWT tokens.
type JWTTokenValidator struct {
	keySet  jwk.Set
	jwksURL string
	devMode bool
}

// NewTokenValidator creates a new JWT token validator with the given JWKS URL.
func NewTokenValidator(jwksURL string) (TokenValidator, error) {
	if jwksURL == "" {
		// If no JWKS URL is provided, use development mode
		return &JWTTokenValidator{
			keySet:  nil,
			jwksURL: "",
			devMode: true,
		}, nil
	}

	// Fetch the JWKS from the URL
	keySet, err := jwk.Fetch(context.Background(), jwksURL)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch JWKS from %s: %w", jwksURL, err)
	}

	return &JWTTokenValidator{
		keySet:  keySet,
		jwksURL: jwksURL,
		devMode: false,
	}, nil
}

// RefreshKeys refreshes the JWKS from the URL.
func (v *JWTTokenValidator) RefreshKeys() error {
	if v.jwksURL == "" {
		return ErrNoJWKS
	}

	keySet, err := jwk.Fetch(context.Background(), v.jwksURL)
	if err != nil {
		return fmt.Errorf("failed to refresh JWKS from %s: %w", v.jwksURL, err)
	}

	v.keySet = keySet
	return nil
}

// ValidateToken validates a JWT token and returns the user ID.
func (v *JWTTokenValidator) ValidateToken(tokenString string) (string, error) {
	// In development mode, extract user ID without validation
	if v.devMode {
		// Parse without verification
		token, _, err := new(jwt.Parser).ParseUnverified(tokenString, &StandardClaims{})
		if err != nil {
			return "", fmt.Errorf("%w: %v", ErrInvalidToken, err)
		}

		if claims, ok := token.Claims.(*StandardClaims); ok {
			if claims.Sub == "" {
				return "", fmt.Errorf("%w: no subject (sub) found in token claims", ErrInvalidToken)
			}
			return claims.Sub, nil
		}

		return "", ErrInvalidToken
	}

	// In production mode, validate the token
	if v.keySet == nil {
		return "", ErrNoJWKS
	}

	// First, parse the token header to get the key ID without validation
	token, _, err := new(jwt.Parser).ParseUnverified(tokenString, &StandardClaims{})
	if err != nil {
		return "", fmt.Errorf("%w: failed to parse token header: %v", ErrInvalidToken, err)
	}

	// Get the key ID from the token header
	kid, ok := token.Header["kid"].(string)
	if !ok {
		return "", fmt.Errorf("%w: token header missing kid", ErrInvalidToken)
	}

	// Find the key with the matching ID
	key, found := v.keySet.LookupKeyID(kid)
	if !found {
		// Try refreshing the keys
		if err := v.RefreshKeys(); err != nil {
			return "", fmt.Errorf("%w: key with ID %s not found and failed to refresh keys: %v", ErrInvalidToken, kid, err)
		}

		// Try again after refresh
		key, found = v.keySet.LookupKeyID(kid)
		if !found {
			// Log all available key IDs for debugging
			var availableKeys []string
			for i := 0; i < v.keySet.Len(); i++ {
				k, _ := v.keySet.Get(i)
				availableKeys = append(availableKeys, k.KeyID())
			}
			return "", fmt.Errorf("%w: key with ID %s not found, available keys: %v", ErrInvalidToken, kid, availableKeys)
		}
	}

	// Get the raw key
	var rawKey interface{}
	if err := key.Raw(&rawKey); err != nil {
		return "", fmt.Errorf("%w: failed to get raw key: %v", ErrInvalidToken, err)
	}

	// Now validate the token with the found key
	validatedToken, err := jwt.ParseWithClaims(
		tokenString,
		&StandardClaims{},
		func(token *jwt.Token) (interface{}, error) {
			return rawKey, nil
		},
	)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrInvalidToken, err)
	}

	claims, ok := validatedToken.Claims.(*StandardClaims)
	if !ok || !validatedToken.Valid {
		return "", ErrInvalidToken
	}

	// Check if token is expired
	if !claims.VerifyExpiresAt(time.Now(), true) {
		return "", ErrExpiredToken
	}

	// Get the subject (user ID)
	if claims.Sub == "" {
		return "", fmt.Errorf("%w: no subject (sub) found in token claims", ErrInvalidToken)
	}

	return claims.Sub, nil
}
