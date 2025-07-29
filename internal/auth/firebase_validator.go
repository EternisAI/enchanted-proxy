package auth

import (
	"context"
	"fmt"

	firebase "firebase.google.com/go/v4"
	"firebase.google.com/go/v4/auth"
	"google.golang.org/api/option"
)

type FirebaseTokenValidator struct {
	authClient *auth.Client
}

func NewFirebaseTokenValidator(ctx context.Context, credJSON string) (*FirebaseTokenValidator, error) {
	opt := option.WithCredentialsJSON([]byte(credJSON))
	app, err := firebase.NewApp(context.Background(), nil, opt)
	if err != nil {
		return nil, fmt.Errorf("error initializing app: %v", err)
	}

	authClient, err := app.Auth(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get Firebase Auth client: %w", err)
	}

	return &FirebaseTokenValidator{
		authClient: authClient,
	}, nil
}

func (f *FirebaseTokenValidator) ValidateToken(tokenString string) (string, error) {
	ctx := context.Background()

	token, err := f.authClient.VerifyIDToken(ctx, tokenString)
	if err != nil {
		return "", err
	}

	// Email if available, fallback to user_id or sub for providers like Twitter.
	if token.Claims["email"] != nil {
		if email, ok := token.Claims["email"].(string); ok && email != "" {
			return email, nil
		}
	}

	if token.Claims["user_id"] != nil {
		if userID, ok := token.Claims["user_id"].(string); ok && userID != "" {
			return userID, nil
		}
	}

	if token.Claims["sub"] != nil {
		if sub, ok := token.Claims["sub"].(string); ok && sub != "" {
			return sub, nil
		}
	}

	return "", fmt.Errorf("no user ID found in Firebase token")
}
