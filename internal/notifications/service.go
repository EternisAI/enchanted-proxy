package notifications

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"cloud.google.com/go/firestore"
	"firebase.google.com/go/v4/messaging"
	"github.com/eternisai/enchanted-proxy/internal/logger"
)

// Service handles sending push notifications via Firebase Cloud Messaging.
type Service struct {
	messagingClient *messaging.Client
	tokenManager    *TokenManager
	logger          *logger.Logger
	enabled         bool
}

// NewService creates a new push notification service.
func NewService(
	messagingClient *messaging.Client,
	firestoreClient *firestore.Client,
	logger *logger.Logger,
	enabled bool,
) *Service {
	tokenManager := NewTokenManager(firestoreClient, logger)

	return &Service{
		messagingClient: messagingClient,
		tokenManager:    tokenManager,
		logger:          logger,
		enabled:         enabled,
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

	log.Info(strings.Repeat("=", 80))
	log.Info("🔔 SENDING PUSH NOTIFICATIONS",
		slog.String("user_id", userID),
		slog.String("type", notification.Data["type"]),
		slog.String("title", notification.Title))
	log.Info(strings.Repeat("=", 80))

	// Get user's push tokens
	tokens, err := s.tokenManager.GetUserTokens(ctx, userID)
	if err != nil {
		log.Warn("failed to retrieve push tokens",
			slog.String("user_id", userID),
			slog.String("error", err.Error()))
		return fmt.Errorf("failed to retrieve push tokens: %w", err)
	}

	log.Info("📦 preparing notification payload",
		slog.String("title", notification.Title),
		slog.Int("body_length", len(notification.Body)),
		slog.Int("data_fields", len(notification.Data)))

	// Send to all devices
	log.Info("📤 sending to devices",
		slog.Int("device_count", len(tokens)))

	var results []SendResult
	successCount := 0
	failureCount := 0

	for idx, tokenInfo := range tokens {
		log.Info(fmt.Sprintf("      ├─ Device %d/%d", idx+1, len(tokens)),
			slog.String("device_id", tokenInfo.DeviceID),
			slog.String("token_prefix", tokenInfo.Token[:min(10, len(tokenInfo.Token))]+"..."))

		result := s.sendToDevice(ctx, tokenInfo, notification)
		results = append(results, result)

		if result.Success {
			successCount++
			log.Info("      └─ ✅ sent successfully",
				slog.String("response", result.Response))
		} else {
			failureCount++
			log.Error("      └─ ❌ failed",
				slog.String("error", result.Error))
		}
	}

	// Log summary
	log.Info(strings.Repeat("-", 80))
	log.Info("📊 NOTIFICATION SUMMARY",
		slog.Int("total_devices", len(tokens)),
		slog.Int("successful", successCount),
		slog.Int("failed", failureCount))

	if successCount == len(tokens) {
		log.Info("✅ ALL NOTIFICATIONS SENT SUCCESSFULLY")
	} else if successCount > 0 {
		log.Warn("⚠️  PARTIAL SUCCESS",
			slog.String("status", fmt.Sprintf("%d/%d sent", successCount, len(tokens))))
	} else {
		log.Error("❌ ALL NOTIFICATIONS FAILED")
	}

	log.Info(strings.Repeat("=", 80))

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

	// Send via FCM
	log.Info("      ├─ 📡 sending via FCM...")
	response, err := s.messagingClient.Send(ctx, message)

	if err != nil {
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

// min returns the minimum of two integers (helper function).
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
