package auth

import (
	"context"
	"fmt"
	"time"

	"cloud.google.com/go/firestore"
	firebase "firebase.google.com/go/v4"
	"firebase.google.com/go/v4/messaging"
	"google.golang.org/api/option"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// FirebaseClient wraps Firebase services.
type FirebaseClient struct {
	firestoreClient *firestore.Client
	messagingClient *messaging.Client
}

// NewFirebaseClient creates a new Firebase client with Firestore access.
func NewFirebaseClient(ctx context.Context, projectID, credJSON string) (*FirebaseClient, error) {
	opt := option.WithCredentialsJSON([]byte(credJSON))

	// Create Firebase config with project ID
	config := &firebase.Config{
		ProjectID: projectID,
	}

	app, err := firebase.NewApp(ctx, config, opt)
	if err != nil {
		return nil, fmt.Errorf("error initializing firebase app: %v", err)
	}

	firestoreClient, err := app.Firestore(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get Firestore client: %w", err)
	}

	messagingClient, err := app.Messaging(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get Messaging client: %w", err)
	}

	return &FirebaseClient{
		firestoreClient: firestoreClient,
		messagingClient: messagingClient,
	}, nil
}

// Close closes the Firestore client.
func (f *FirebaseClient) Close() error {
	if f.firestoreClient != nil {
		return f.firestoreClient.Close()
	}
	return nil
}

// GetFirestoreClient returns the Firestore client instance
func (f *FirebaseClient) GetFirestoreClient() *firestore.Client {
	return f.firestoreClient
}

// GetMessagingClient returns the Messaging client instance for sending push notifications
func (f *FirebaseClient) GetMessagingClient() *messaging.Client {
	return f.messagingClient
}

// DeepResearchUsage represents a user's deep research usage record
type DeepResearchUsage struct {
	UserID                  string    `firestore:"user_id"`
	HasUsedFreeDeepResearch bool      `firestore:"has_used_free_deep_research"`
	FirstUsedAt             time.Time `firestore:"first_used_at"`
	LastUsedAt              time.Time `firestore:"last_used_at"`
	UsageCount              int64     `firestore:"usage_count"`
}

// HasUsedFreeDeepResearch checks if a freemium user has already used deep research.
func (f *FirebaseClient) HasUsedFreeDeepResearch(ctx context.Context, userID string) (bool, error) {
	docRef := f.firestoreClient.Collection("deep_research_usage").Doc(userID)
	doc, err := docRef.Get(ctx)
	if err != nil {
		// If document doesn't exist, user hasn't used it yet
		if status.Code(err) == codes.NotFound {
			return false, nil
		}
		return false, fmt.Errorf("failed to get deep research usage: %w", err)
	}

	var usage DeepResearchUsage
	if err := doc.DataTo(&usage); err != nil {
		return false, fmt.Errorf("failed to parse deep research usage: %w", err)
	}

	return usage.HasUsedFreeDeepResearch, nil
}

// MarkFreeDeepResearchUsed marks that a freemium user has used their free deep research.
func (f *FirebaseClient) MarkFreeDeepResearchUsed(ctx context.Context, userID string) error {
	docRef := f.firestoreClient.Collection("deep_research_usage").Doc(userID)

	// Check if document exists
	doc, err := docRef.Get(ctx)
	now := time.Now()

	if err != nil {
		// Document doesn't exist, create new one
		usage := DeepResearchUsage{
			UserID:                  userID,
			HasUsedFreeDeepResearch: true,
			FirstUsedAt:             now,
			LastUsedAt:              now,
			UsageCount:              1,
		}
		_, err := docRef.Set(ctx, usage)
		if err != nil {
			return fmt.Errorf("failed to create deep research usage record: %w", err)
		}
		return nil
	}

	// Document exists, update it
	var usage DeepResearchUsage
	if err := doc.DataTo(&usage); err != nil {
		return fmt.Errorf("failed to parse existing usage record: %w", err)
	}

	// Update the record
	_, err = docRef.Set(ctx, map[string]interface{}{
		"has_used_free_deep_research": true,
		"last_used_at":                now,
		"usage_count":                 usage.UsageCount + 1,
	}, firestore.MergeAll)
	if err != nil {
		return fmt.Errorf("failed to update deep research usage record: %w", err)
	}

	return nil
}

// IncrementDeepResearchUsage increments usage counter for pro users (for analytics).
func (f *FirebaseClient) IncrementDeepResearchUsage(ctx context.Context, userID string) error {
	docRef := f.firestoreClient.Collection("deep_research_usage").Doc(userID)
	now := time.Now()

	doc, err := docRef.Get(ctx)
	if err != nil {
		// Create new record for pro user
		usage := DeepResearchUsage{
			UserID:                  userID,
			HasUsedFreeDeepResearch: false, // Pro users don't count as "free" usage
			FirstUsedAt:             now,
			LastUsedAt:              now,
			UsageCount:              1,
		}
		_, err := docRef.Set(ctx, usage)
		return err
	}

	var usage DeepResearchUsage
	if err := doc.DataTo(&usage); err != nil {
		return fmt.Errorf("failed to parse usage record: %w", err)
	}

	// Update usage count and last used time
	_, err = docRef.Set(ctx, map[string]interface{}{
		"last_used_at": now,
		"usage_count":  usage.UsageCount + 1,
	}, firestore.MergeAll)

	return err
}

// SaveDeepResearchCompletion saves completion data when deep research finishes successfully
// Note: This only saves completion metadata. Usage tracking (has_used_free_deep_research)
// should be handled separately via MarkFreeDeepResearchUsed or IncrementDeepResearchUsage.
func (f *FirebaseClient) SaveDeepResearchCompletion(ctx context.Context, userID, chatID string) error {
	docRef := f.firestoreClient.Collection("deep_research_usage").Doc(userID)
	now := time.Now()

	// Always use merge to avoid overwriting existing fields like has_used_free_deep_research
	_, err := docRef.Set(ctx, map[string]interface{}{
		"user_id":                userID,
		"last_completed_chat_id": chatID,
		"completed_at":           now,
	}, firestore.MergeAll)
	if err != nil {
		return fmt.Errorf("failed to save deep research completion record: %w", err)
	}

	return nil
}

// DeepResearchSessionState represents the state of a deep research session.
type DeepResearchSessionState struct {
	UserID      string    `firestore:"user_id"`
	ChatID      string    `firestore:"chat_id"`
	State       string    `firestore:"state"` // in_progress, clarify, error, complete
	CreatedAt   time.Time `firestore:"created_at"`
	UpdatedAt   time.Time `firestore:"updated_at"`
	CompletedAt time.Time `firestore:"completed_at,omitempty"`
}

// DeepResearchState represents the state of a deep research session on a chat document.
type DeepResearchState struct {
	StartedAt     time.Time          `firestore:"startedAt" json:"startedAt"`
	Status        string             `firestore:"status" json:"status"`                                   // "in_progress", "clarify", "error", "complete"
	ThinkingState string             `firestore:"thinkingState,omitempty" json:"thinkingState,omitempty"` // Latest progress message
	Error         *DeepResearchError `firestore:"error,omitempty" json:"error,omitempty"`
}

// DeepResearchError contains error information for a failed deep research session.
type DeepResearchError struct {
	UnderlyingError string `firestore:"underlyingError" json:"underlyingError"` // Technical error details
	UserMessage     string `firestore:"userMessage" json:"userMessage"`         // User-friendly error message
}

// GetSessionState retrieves the current state of a deep research session.
func (f *FirebaseClient) GetSessionState(ctx context.Context, userID, chatID string) (*DeepResearchSessionState, error) {
	// Use underscore as separator since forward slash is not allowed in Firestore document IDs
	sessionID := fmt.Sprintf("%s__%s", userID, chatID)
	docRef := f.firestoreClient.Collection("deep_research_sessions").Doc(sessionID)
	doc, err := docRef.Get(ctx)
	if err != nil {
		// If document doesn't exist, session hasn't been created yet
		if status.Code(err) == codes.NotFound {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to get session state: %w", err)
	}

	var state DeepResearchSessionState
	if err := doc.DataTo(&state); err != nil {
		return nil, fmt.Errorf("failed to parse session state: %w", err)
	}

	return &state, nil
}

// UpdateSessionState updates the state of a deep research session.
func (f *FirebaseClient) UpdateSessionState(ctx context.Context, userID, chatID, state string) error {
	// Use underscore as separator since forward slash is not allowed in Firestore document IDs
	sessionID := fmt.Sprintf("%s__%s", userID, chatID)
	docRef := f.firestoreClient.Collection("deep_research_sessions").Doc(sessionID)
	now := time.Now()

	// Check if document exists
	_, err := docRef.Get(ctx)
	if err != nil {
		// Document doesn't exist, create new one
		if status.Code(err) == codes.NotFound {
			sessionState := DeepResearchSessionState{
				UserID:    userID,
				ChatID:    chatID,
				State:     state,
				CreatedAt: now,
				UpdatedAt: now,
			}

			// Set completed_at if state is complete
			if state == "complete" {
				sessionState.CompletedAt = now
			}

			_, err := docRef.Set(ctx, sessionState)
			if err != nil {
				return fmt.Errorf("failed to create session state: %w", err)
			}
			return nil
		}
		return fmt.Errorf("failed to get session state: %w", err)
	}

	// Document exists, update it
	updateData := map[string]interface{}{
		"state":      state,
		"updated_at": now,
	}

	// Set completed_at when state is complete
	if state == "complete" {
		updateData["completed_at"] = now
	}

	_, err = docRef.Set(ctx, updateData, firestore.MergeAll)
	if err != nil {
		return fmt.Errorf("failed to update session state: %w", err)
	}

	return nil
}

// GetActiveSessionsForUser retrieves all active (non-complete, non-error) sessions for a user.
func (f *FirebaseClient) GetActiveSessionsForUser(ctx context.Context, userID string) ([]DeepResearchSessionState, error) {
	query := f.firestoreClient.Collection("deep_research_sessions").
		Where("user_id", "==", userID).
		Where("state", "in", []string{"in_progress", "clarify"})

	docs, err := query.Documents(ctx).GetAll()
	if err != nil {
		return nil, fmt.Errorf("failed to get active sessions: %w", err)
	}

	var sessions []DeepResearchSessionState
	for _, doc := range docs {
		var session DeepResearchSessionState
		if err := doc.DataTo(&session); err != nil {
			return nil, fmt.Errorf("failed to parse session: %w", err)
		}
		sessions = append(sessions, session)
	}

	return sessions, nil
}

// GetCompletedSessionCountForUser returns the number of completed deep research sessions for a user.
func (f *FirebaseClient) GetCompletedSessionCountForUser(ctx context.Context, userID string) (int, error) {
	query := f.firestoreClient.Collection("deep_research_sessions").
		Where("user_id", "==", userID).
		Where("state", "==", "complete")

	docs, err := query.Documents(ctx).GetAll()
	if err != nil {
		return 0, fmt.Errorf("failed to get completed sessions count: %w", err)
	}

	return len(docs), nil
}

// UpdateChatDeepResearchState updates the deep research state on a chat document.
// This provides easy UI access to deep research status without querying the sessions collection.
func (f *FirebaseClient) UpdateChatDeepResearchState(ctx context.Context, userID, chatID string, state *DeepResearchState) error {
	if state == nil {
		return fmt.Errorf("state cannot be nil")
	}

	docRef := f.firestoreClient.Collection("users").Doc(userID).Collection("chats").Doc(chatID)

	// Use MergeAll to preserve other chat fields like encryptedTitle, etc.
	_, err := docRef.Set(ctx, map[string]interface{}{
		"deepResearchState": state,
	}, firestore.MergeAll)

	if err != nil {
		return fmt.Errorf("failed to update chat deep research state: %w", err)
	}

	return nil
}

// GetChatDeepResearchState retrieves the deep research state from a chat document.
// Returns nil if the chat doesn't have a deep research state.
func (f *FirebaseClient) GetChatDeepResearchState(ctx context.Context, userID, chatID string) (*DeepResearchState, error) {
	docRef := f.firestoreClient.Collection("users").Doc(userID).Collection("chats").Doc(chatID)

	doc, err := docRef.Get(ctx)
	if err != nil {
		if status.Code(err) == codes.NotFound {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to get chat document: %w", err)
	}

	// Get the deepResearchState field
	stateData := doc.Data()["deepResearchState"]
	if stateData == nil {
		return nil, nil
	}

	// Convert to DeepResearchState struct
	var state DeepResearchState
	stateMap, ok := stateData.(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("deepResearchState is not a map")
	}

	// Parse the fields
	if startedAt, ok := stateMap["startedAt"].(time.Time); ok {
		state.StartedAt = startedAt
	}
	if status, ok := stateMap["status"].(string); ok {
		state.Status = status
	}
	if thinkingState, ok := stateMap["thinkingState"].(string); ok {
		state.ThinkingState = thinkingState
	}
	if errorData, ok := stateMap["error"].(map[string]interface{}); ok {
		drErr := &DeepResearchError{}
		if underlying, ok := errorData["underlyingError"].(string); ok {
			drErr.UnderlyingError = underlying
		}
		if userMsg, ok := errorData["userMessage"].(string); ok {
			drErr.UserMessage = userMsg
		}
		if drErr.UnderlyingError != "" || drErr.UserMessage != "" {
			state.Error = drErr
		}
	}

	return &state, nil
}
