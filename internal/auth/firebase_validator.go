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

// ExtractIdentity checks that the ID token is valid and returns the holder's Firebase UID
// from "sub" (which according to Firebase docs is always present:
// https://firebase.google.com/docs/auth/admin/verify-id-tokens#go), with the sign-in
// provider, email and email-verified flag from the same token.
func (f *FirebaseTokenValidator) ExtractIdentity(tokenString string) (Identity, error) {
	token, err := f.authClient.VerifyIDToken(context.Background(), tokenString)
	if err != nil {
		return Identity{}, err
	}
	if token.Subject == "" {
		return Identity{}, fmt.Errorf("no Firebase UID (sub claim) found in token")
	}
	return identityFromFirebaseToken(token), nil
}

// identityFromFirebaseToken reads the Identity out of a verified Firebase token.
func identityFromFirebaseToken(token *auth.Token) Identity {
	id := Identity{
		UserID:         token.Subject,
		SignInProvider: token.Firebase.SignInProvider,
	}
	if email, ok := token.Claims["email"].(string); ok {
		id.Email = email
	}
	if verified, ok := token.Claims["email_verified"].(bool); ok {
		id.EmailVerified = verified
	}
	return id
}
