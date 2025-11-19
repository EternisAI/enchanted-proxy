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

// Checks that this user's auth token is valid and extracts + returns the user's Firebase ID
// from "sub" - which according to Firebase docs should always be present: https://firebase.google.com/docs/auth/admin/verify-id-tokens#go
func (f *FirebaseTokenValidator) ExtractUserID(tokenString string) (string, error) {
	ctx := context.Background()

	token, err := f.authClient.VerifyIDToken(ctx, tokenString)
	if err != nil {
		return "", err
	}

	sub := token.Subject
	if sub != "" {
		return sub, nil
	}

	return "", fmt.Errorf("no Firebase UID (sub claim) found in token")
}
