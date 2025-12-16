package notifications

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"

	"cloud.google.com/go/firestore"
	"firebase.google.com/go/v4/messaging"
	"github.com/eternisai/enchanted-proxy/internal/logger"
	"golang.org/x/oauth2/google"
)

// Service handles sending push notifications via Firebase Cloud Messaging.
type Service struct {
	messagingClient *messaging.Client
	tokenManager    *TokenManager
	logger          *logger.Logger
	enabled         bool
	credJSON        string // For debug curl generation
	projectID       string // For debug curl generation
}

// NewService creates a new push notification service.
func NewService(
	messagingClient *messaging.Client,
	firestoreClient *firestore.Client,
	logger *logger.Logger,
	enabled bool,
	credJSON string,
	projectID string,
) *Service {
	tokenManager := NewTokenManager(firestoreClient, logger)

	return &Service{
		messagingClient: messagingClient,
		tokenManager:    tokenManager,
		logger:          logger,
		enabled:         enabled,
		credJSON:        credJSON,
		projectID:       projectID,
	}
}

// SendDeepResearchCompletionNotification sends a notification when Deep Research completes.
func (s *Service) SendDeepResearchCompletionNotification(
	ctx context.Context,
	userID string,
	chatID string,
) error {
	notification := CompletionNotification{
		Title: "Deep Research Complete",
		Body:  "Your research has finished. Tap to review the full response.",
		Data: map[string]string{
			"user_id": userID,
			"chat_id": chatID,
			"type":    string(TypeDeepResearch),
		},
	}

	return s.sendNotification(ctx, userID, notification)
}

// SendGPT5ProCompletionNotification sends a notification when GPT-5 Pro response completes.
func (s *Service) SendGPT5ProCompletionNotification(
	ctx context.Context,
	userID string,
	chatID string,
	messageID string,
) error {
	notification := CompletionNotification{
		Title: "Response Ready",
		Body:  "Your response has finished generating. Tap to review the full response.",
		Data: map[string]string{
			"user_id":    userID,
			"chat_id":    chatID,
			"message_id": messageID,
			"type":       string(TypeGPT5Pro),
		},
	}

	return s.sendNotification(ctx, userID, notification)
}

// sendNotification sends a notification to all of a user's registered devices.
func (s *Service) sendNotification(
	ctx context.Context,
	userID string,
	notification CompletionNotification,
) error {
	log := s.logger.WithContext(ctx).WithComponent("push-notifications")

	// Check if notifications are enabled
	if !s.enabled {
		log.Debug("push notifications disabled, skipping",
			slog.String("user_id", userID),
			slog.String("notification_type", notification.Data["type"]))
		return nil
	}

	// Get user's push tokens
	tokens, err := s.tokenManager.GetUserTokens(ctx, userID)
	if err != nil {
		log.Warn("failed to retrieve push tokens",
			slog.String("user_id", userID),
			slog.String("type", notification.Data["type"]))
		return fmt.Errorf("failed to retrieve push tokens: %w", err)
	}

	log.Info("sending push notification",
		slog.String("user_id", userID),
		slog.String("type", notification.Data["type"]),
		slog.Int("devices", len(tokens)))

	var results []SendResult
	successCount := 0
	failureCount := 0
	var lastError string

	for _, tokenInfo := range tokens {
		result := s.sendToDevice(ctx, tokenInfo, notification)
		results = append(results, result)

		if result.Success {
			successCount++
		} else {
			failureCount++
			lastError = result.Error
		}
	}

	// Log summary based on result
	if successCount == len(tokens) {
		log.Info("push notification sent",
			slog.String("user_id", userID),
			slog.String("type", notification.Data["type"]),
			slog.Int("devices", successCount))
	} else if successCount > 0 {
		log.Warn("push notification partially sent",
			slog.String("user_id", userID),
			slog.String("type", notification.Data["type"]),
			slog.Int("sent", successCount),
			slog.Int("failed", failureCount))
	} else {
		log.Error("push notification failed",
			slog.String("user_id", userID),
			slog.String("type", notification.Data["type"]),
			slog.Int("devices", len(tokens)),
			slog.String("error", lastError))
	}

	// Return error only if all notifications failed
	if failureCount == len(tokens) {
		return fmt.Errorf("all %d notification(s) failed", failureCount)
	}

	return nil
}

// sendToDevice sends a notification to a single device.
func (s *Service) sendToDevice(
	ctx context.Context,
	tokenInfo TokenInfo,
	notification CompletionNotification,
) SendResult {
	log := s.logger.WithContext(ctx).WithComponent("push-notifications")

	// Create FCM message
	message := &messaging.Message{
		Notification: &messaging.Notification{
			Title: notification.Title,
			Body:  notification.Body,
		},
		Data:  notification.Data,
		Token: tokenInfo.Token,
	}

	// Send via direct HTTP using manual OAuth (bypassing SDK due to Nitro Enclave issues)
	response, err := s.sendViaDirectHTTP(ctx, message)

	if err != nil {
		// Generate debug curl command that can be copy-pasted to test
		debugCurl := GenerateDebugCurl(ctx, s.credJSON, s.projectID, message)

		// Log detailed error information for debugging
		log.Error("FCM send failed - CHECK STARTUP LOGS FOR CREDENTIALS",
			slog.String("error", err.Error()),
			slog.String("error_type", fmt.Sprintf("%T", err)),
			slog.String("token_prefix", tokenInfo.Token[:min(10, len(tokenInfo.Token))]),
			slog.String("notification_type", notification.Data["type"]),
			slog.String("messaging_client_status", fmt.Sprintf("%p", s.messagingClient)),
			slog.String("debug_curl", debugCurl))

		return SendResult{
			Token:   tokenInfo.Token[:min(10, len(tokenInfo.Token))] + "...",
			Success: false,
			Error:   err.Error(),
		}
	}

	return SendResult{
		Token:    tokenInfo.Token[:min(10, len(tokenInfo.Token))] + "...",
		Success:  true,
		Response: response,
	}
}

// sendViaDirectHTTP sends FCM notification using direct HTTP REST API with manual OAuth.
// This bypasses the Firebase SDK which has issues in Nitro Enclave environment.
func (s *Service) sendViaDirectHTTP(ctx context.Context, message *messaging.Message) (string, error) {
	log := s.logger.WithContext(ctx).WithComponent("fcm-direct-http")

	// Generate OAuth token manually (we verified this works in Nitro Enclave)
	creds, err := google.CredentialsFromJSON(ctx, []byte(s.credJSON),
		"https://www.googleapis.com/auth/firebase.messaging")
	if err != nil {
		return "", fmt.Errorf("failed to parse credentials: %w", err)
	}

	token, err := creds.TokenSource.Token()
	if err != nil {
		return "", fmt.Errorf("failed to get OAuth token: %w", err)
	}

	log.Info("generated OAuth token for direct FCM HTTP request",
		slog.String("token_prefix", token.AccessToken[:30]),
		slog.String("token_suffix", token.AccessToken[len(token.AccessToken)-20:]),
		slog.Time("expiry", token.Expiry))

	// Build FCM v1 API request body
	requestBody := map[string]interface{}{
		"message": map[string]interface{}{
			"token": message.Token,
			"notification": map[string]interface{}{
				"title": message.Notification.Title,
				"body":  message.Notification.Body,
			},
			"data": message.Data,
		},
	}

	bodyBytes, err := json.Marshal(requestBody)
	if err != nil {
		return "", fmt.Errorf("failed to marshal request body: %w", err)
	}

	// Create HTTP request
	url := fmt.Sprintf("https://fcm.googleapis.com/v1/projects/%s/messages:send", s.projectID)
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewBuffer(bodyBytes))
	if err != nil {
		return "", fmt.Errorf("failed to create HTTP request: %w", err)
	}

	// Set headers
	req.Header.Set("Authorization", "Bearer "+token.AccessToken)
	req.Header.Set("Content-Type", "application/json")

	log.Info("sending direct HTTP request to FCM",
		slog.String("url", url),
		slog.String("device_token_prefix", message.Token[:min(20, len(message.Token))]))

	// Make the request
	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("HTTP request failed: %w", err)
	}
	defer resp.Body.Close()

	// Read response body
	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read response body: %w", err)
	}

	log.Info("received FCM response",
		slog.Int("status_code", resp.StatusCode),
		slog.String("status", resp.Status),
		slog.String("response_body", string(responseBody)))

	// Check for errors
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("FCM API error (status %d): %s", resp.StatusCode, string(responseBody))
	}

	// Parse response to get message ID
	var fcmResponse struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(responseBody, &fcmResponse); err != nil {
		return "", fmt.Errorf("failed to parse FCM response: %w", err)
	}

	log.Info("FCM notification sent successfully via direct HTTP",
		slog.String("message_id", fcmResponse.Name))

	return fcmResponse.Name, nil
}

// min returns the minimum of two integers (helper function).
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
