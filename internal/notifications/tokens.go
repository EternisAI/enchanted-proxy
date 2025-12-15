package notifications

import (
	"context"
	"fmt"
	"log/slog"

	"cloud.google.com/go/firestore"
	"github.com/eternisai/enchanted-proxy/internal/logger"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TokenManager handles reading push notification tokens from Firestore.
type TokenManager struct {
	firestoreClient *firestore.Client
	logger          *logger.Logger
}

// NewTokenManager creates a new token manager.
func NewTokenManager(firestoreClient *firestore.Client, logger *logger.Logger) *TokenManager {
	return &TokenManager{
		firestoreClient: firestoreClient,
		logger:          logger,
	}
}

// GetUserTokens retrieves all push notification tokens for a user from Firestore.
// Tokens are stored at /push_tokens/{user_id}/ with structure:
//
//	{
//	  tokens: {
//	    deviceId1: {token: "fcm_token_...", deviceId: "device1", lastUpdatedAt: timestamp},
//	    deviceId2: {...}
//	  }
//	}
func (tm *TokenManager) GetUserTokens(ctx context.Context, userID string) ([]TokenInfo, error) {
	log := tm.logger.WithContext(ctx).WithComponent("token-manager")

	log.Info("fetching push tokens from Firestore",
		slog.String("user_id", userID),
		slog.String("path", fmt.Sprintf("push_tokens/%s", userID)))

	docRef := tm.firestoreClient.Collection("push_tokens").Doc(userID)
	doc, err := docRef.Get(ctx)

	if err != nil {
		if status.Code(err) == codes.NotFound {
			log.Warn("no push tokens document found for user",
				slog.String("user_id", userID))
			return nil, fmt.Errorf("no push tokens found for user %s", userID)
		}
		log.Error("failed to fetch push tokens document",
			slog.String("user_id", userID),
			slog.String("error", err.Error()))
		return nil, fmt.Errorf("failed to fetch push tokens: %w", err)
	}

	data := doc.Data()
	tokensData, ok := data["tokens"]
	if !ok {
		log.Warn("tokens field not found in push tokens document",
			slog.String("user_id", userID))
		return nil, fmt.Errorf("tokens field not found for user %s", userID)
	}

	// Parse tokens map: {deviceId: {token, deviceId, lastUpdatedAt}, ...}
	tokensMap, ok := tokensData.(map[string]interface{})
	if !ok {
		log.Error("tokens field is not a map",
			slog.String("user_id", userID),
			slog.String("type", fmt.Sprintf("%T", tokensData)))
		return nil, fmt.Errorf("invalid tokens data structure")
	}

	if len(tokensMap) == 0 {
		log.Warn("empty tokens map for user",
			slog.String("user_id", userID))
		return nil, fmt.Errorf("no tokens available for user %s", userID)
	}

	// Convert map to slice of TokenInfo
	var tokens []TokenInfo
	for deviceID, tokenData := range tokensMap {
		tokenMap, ok := tokenData.(map[string]interface{})
		if !ok {
			log.Warn("skipping invalid token entry",
				slog.String("device_id", deviceID),
				slog.String("type", fmt.Sprintf("%T", tokenData)))
			continue
		}

		token, ok := tokenMap["token"].(string)
		if !ok || token == "" {
			log.Warn("skipping token entry with missing token field",
				slog.String("device_id", deviceID))
			continue
		}

		tokenInfo := TokenInfo{
			Token:    token,
			DeviceID: deviceID,
		}

		// Optional: extract lastUpdatedAt if present
		if lastUpdated, ok := tokenMap["lastUpdatedAt"].(string); ok {
			tokenInfo.LastUpdatedAt = lastUpdated
		}

		tokens = append(tokens, tokenInfo)
	}

	if len(tokens) == 0 {
		log.Warn("no valid tokens found after parsing",
			slog.String("user_id", userID),
			slog.Int("raw_entries", len(tokensMap)))
		return nil, fmt.Errorf("no valid tokens found for user %s", userID)
	}

	log.Info("successfully retrieved push tokens",
		slog.String("user_id", userID),
		slog.Int("token_count", len(tokens)))

	return tokens, nil
}
