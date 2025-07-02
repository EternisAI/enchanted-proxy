package auth

import (
	"context"
	"fmt"

	firebase "firebase.google.com/go/v4"
	"firebase.google.com/go/v4/auth"
)

type FirebaseTokenValidator struct {
	authClient *auth.Client
	projectID  string
}

func NewFirebaseTokenValidator(ctx context.Context, projectID string) (*FirebaseTokenValidator, error) {
	app, err := firebase.NewApp(ctx, &firebase.Config{ProjectID: projectID})
	if err != nil {
		return nil, fmt.Errorf("failed to initialize Firebase app: %w", err)
	}

	authClient, err := app.Auth(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get Firebase Auth client: %w", err)
	}

	return &FirebaseTokenValidator{
		authClient: authClient,
		projectID:  projectID,
	}, nil
}


func (f *FirebaseTokenValidator) ValidateToken(tokenString string) (string, error) {
	ctx := context.Background()

	token, err := f.authClient.VerifyIDToken(ctx, tokenString)
	if err != nil {
		return "", err
	}

	if token.UID == "" {
		return "", fmt.Errorf("No user ID found in Firebase token")
	}

	return token.UID, nil
}