package auth

import (
	"context"
	"fmt"
	"time"

	"cloud.google.com/go/firestore"
	firebase "firebase.google.com/go/v4"
	"google.golang.org/api/option"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// FirebaseClient wraps Firebase services
type FirebaseClient struct {
	firestoreClient *firestore.Client
}

// NewFirebaseClient creates a new Firebase client with Firestore access
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

	return &FirebaseClient{
		firestoreClient: firestoreClient,
	}, nil
}

// Close closes the Firestore client
func (f *FirebaseClient) Close() error {
	if f.firestoreClient != nil {
		return f.firestoreClient.Close()
	}
	return nil
}

// DeepResearchUsage represents a user's deep research usage record
type DeepResearchUsage struct {
	UserID                  string    `firestore:"user_id"`
	HasUsedFreeDeepResearch bool      `firestore:"has_used_free_deep_research"`
	FirstUsedAt             time.Time `firestore:"first_used_at"`
	LastUsedAt              time.Time `firestore:"last_used_at"`
	UsageCount              int64     `firestore:"usage_count"`
}

// HasUsedFreeDeepResearch checks if a freemium user has already used deep research
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

// MarkFreeDeepResearchUsed marks that a freemium user has used their free deep research
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

// IncrementDeepResearchUsage increments usage counter for pro users (for analytics)
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
// should be handled separately via MarkFreeDeepResearchUsed or IncrementDeepResearchUsage
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
