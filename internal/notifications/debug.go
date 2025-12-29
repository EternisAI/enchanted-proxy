package notifications

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"firebase.google.com/go/v4/messaging"
	"golang.org/x/oauth2/google"
)

// GenerateDebugCurl creates a curl command that replicates the FCM request for debugging.
// This allows copying and running the exact request that failed.
func GenerateDebugCurl(ctx context.Context, credJSON string, projectID string, message *messaging.Message) string {
	// Get OAuth token from credentials
	creds, err := google.CredentialsFromJSON(
		ctx,
		[]byte(credJSON),
		"https://www.googleapis.com/auth/firebase.messaging",
	)
	if err != nil {
		return fmt.Sprintf("# ERROR: Failed to parse credentials: %v", err)
	}

	token, err := creds.TokenSource.Token()
	if err != nil {
		return fmt.Sprintf("# ERROR: Failed to get OAuth token: %v", err)
	}

	// Build FCM v1 API request payload
	payload := map[string]interface{}{
		"message": map[string]interface{}{
			"token": message.Token,
			"notification": map[string]interface{}{
				"title": message.Notification.Title,
				"body":  message.Notification.Body,
			},
			"data": message.Data,
		},
	}

	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		return fmt.Sprintf("# ERROR: Failed to marshal payload: %v", err)
	}

	// Format as curl command
	curl := fmt.Sprintf(`curl -X POST \
  'https://fcm.googleapis.com/v1/projects/%s/messages:send' \
  -H 'Authorization: Bearer %s' \
  -H 'Content-Type: application/json' \
  -d '%s'`,
		projectID,
		token.AccessToken,
		strings.ReplaceAll(string(payloadJSON), "'", "\\'"))

	return curl
}
