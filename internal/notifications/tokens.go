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

	docRef := tm.firestoreClient.Collection("push_tokens").Doc(userID)
	doc, err := docRef.Get(ctx)

	if err != nil {
		if status.Code(err) == codes.NotFound {
			log.Debug("no push tokens found",
				slog.String("user_id", userID))
			return nil, fmt.Errorf("no push tokens found for user %s", userID)
		}
		log.Warn("failed to fetch push tokens",
			slog.String("user_id", userID),
			slog.String("error", err.Error()))
		return nil, fmt.Errorf("failed to fetch push tokens: %w", err)
	}

	data := doc.Data()
	tokensData, ok := data["tokens"]
	if !ok {
		log.Debug("tokens field not found",
			slog.String("user_id", userID))
		return nil, fmt.Errorf("tokens field not found for user %s", userID)
	}

	// Parse tokens map: {deviceId: {token, deviceId, lastUpdatedAt}, ...}
	tokensMap, ok := tokensData.(map[string]interface{})
	if !ok {
		log.Warn("invalid tokens data structure",
			slog.String("user_id", userID))
		return nil, fmt.Errorf("invalid tokens data structure")
	}

	if len(tokensMap) == 0 {
		log.Debug("no tokens available",
			slog.String("user_id", userID))
		return nil, fmt.Errorf("no tokens available for user %s", userID)
	}

	// Convert map to slice of TokenInfo
	var tokens []TokenInfo
	for deviceID, tokenData := range tokensMap {
		tokenMap, ok := tokenData.(map[string]interface{})
		if !ok {
			continue
		}

		token, ok := tokenMap["token"].(string)
		if !ok || token == "" {
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
		log.Debug("no valid tokens found",
			slog.String("user_id", userID))
		return nil, fmt.Errorf("no valid tokens found for user %s", userID)
	}

	return tokens, nil
}
