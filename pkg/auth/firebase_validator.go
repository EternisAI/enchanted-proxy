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

	if token.Claims["email"] == nil {
		return "", fmt.Errorf("no user email found in Firebase token")
	}

	email, ok := token.Claims["email"].(string)
	if !ok {
		return "", fmt.Errorf("invalid email in Firebase token")
	}

	return email, nil
}
