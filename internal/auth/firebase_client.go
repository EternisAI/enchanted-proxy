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
	UserID            string               `firestore:"user_id"`
	FirstUsedAt       time.Time            `firestore:"first_used_at"`
	LastUsedAt        time.Time            `firestore:"last_used_at"`
	UsageCount        int64                `firestore:"usage_count"`
	CompletedSessions map[string]time.Time `firestore:"completed_sessions"` // Map of chat_id to completion timestamp
}

// DeepResearchQuotaStatus represents the quota status for a user
type DeepResearchQuotaStatus struct {
	IsPro              bool       `json:"is_pro"`
	Used               int        `json:"used"`
	Limit              int        `json:"limit"`
	Remaining          int        `json:"remaining"`
	QuotaExceeded      bool       `json:"quota_exceeded"`
	ResetsAt           *time.Time `json:"resets_at,omitempty"` // Only for pro users
	ErrorCode          string     `json:"error_code,omitempty"`
	ErrorMessage       string     `json:"error_message,omitempty"`
	CompletedSessions  int        `json:"completed_sessions"` // Total completed sessions (all time)
	CompletedThisMonth int        `json:"completed_this_month,omitempty"`
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
			UserID:      userID,
			FirstUsedAt: now,
			LastUsedAt:  now,
			UsageCount:  1,
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
		"last_used_at": now,
		"usage_count":  usage.UsageCount + 1,
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
			UserID:      userID,
			FirstUsedAt: now,
			LastUsedAt:  now,
			UsageCount:  1,
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
// This stores the completion in the completed_sessions map and updates metadata
// Note: Usage tracking should be handled separately via MarkFreeDeepResearchUsed or IncrementDeepResearchUsage
func (f *FirebaseClient) SaveDeepResearchCompletion(ctx context.Context, userID, chatID string) error {
	docRef := f.firestoreClient.Collection("deep_research_usage").Doc(userID)
	now := time.Now()

	// Get existing document to access the completed_sessions map
	doc, err := docRef.Get(ctx)
	completedSessions := make(map[string]time.Time)

	if err == nil {
		// Document exists, get the current completed_sessions map
		var usage DeepResearchUsage
		if err := doc.DataTo(&usage); err == nil && usage.CompletedSessions != nil {
			completedSessions = usage.CompletedSessions
		}
	}

	// Add the new session to the map
	completedSessions[chatID] = now

	// Always use merge to avoid overwriting existing fields
	_, err = docRef.Set(ctx, map[string]interface{}{
		"user_id":                userID,
		"last_completed_chat_id": chatID,
		"completed_at":           now,
		"completed_sessions":     completedSessions,
	}, firestore.MergeAll)

	if err != nil {
		return fmt.Errorf("failed to save deep research completion record: %w", err)
	}

	return nil
}

// DeepResearchSessionState represents the state of a deep research session
type DeepResearchSessionState struct {
	UserID      string    `firestore:"user_id"`
	ChatID      string    `firestore:"chat_id"`
	State       string    `firestore:"state"` // in_progress, clarify, error, complete
	CreatedAt   time.Time `firestore:"created_at"`
	UpdatedAt   time.Time `firestore:"updated_at"`
	CompletedAt time.Time `firestore:"completed_at,omitempty"`
}

// GetSessionState retrieves the current state of a deep research session
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

// UpdateSessionState updates the state of a deep research session
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

// GetActiveSessionsForUser retrieves all active (non-complete, non-error) sessions for a user
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

// GetCompletedSessionCountForUser returns the number of completed deep research sessions for a user
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

// GetDeepResearchQuotaStatus calculates and returns the quota status for a user
// For free users: 1 session lifetime
// For pro users: 2 sessions per calendar month
func (f *FirebaseClient) GetDeepResearchQuotaStatus(ctx context.Context, userID string, isPro bool) (*DeepResearchQuotaStatus, error) {
	docRef := f.firestoreClient.Collection("deep_research_usage").Doc(userID)
	doc, err := docRef.Get(ctx)

	var completedSessions map[string]time.Time
	totalCompleted := 0

	if err == nil {
		// Document exists, get the completed_sessions map
		var usage DeepResearchUsage
		if err := doc.DataTo(&usage); err == nil && usage.CompletedSessions != nil {
			completedSessions = usage.CompletedSessions
			totalCompleted = len(completedSessions)
		}
	} else if status.Code(err) != codes.NotFound {
		return nil, fmt.Errorf("failed to get deep research usage: %w", err)
	}

	now := time.Now()
	status := &DeepResearchQuotaStatus{
		IsPro:             isPro,
		CompletedSessions: totalCompleted,
	}

	if isPro {
		// Pro users: 2 sessions per calendar month
		status.Limit = 2

		// Calculate sessions completed in current calendar month
		currentMonthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location())
		nextMonthStart := currentMonthStart.AddDate(0, 1, 0)
		status.ResetsAt = &nextMonthStart

		completedThisMonth := 0
		for _, completionTime := range completedSessions {
			if completionTime.After(currentMonthStart) && completionTime.Before(nextMonthStart) {
				completedThisMonth++
			}
		}

		status.Used = completedThisMonth
		status.CompletedThisMonth = completedThisMonth
		status.Remaining = status.Limit - status.Used
		if status.Remaining < 0 {
			status.Remaining = 0
		}

		status.QuotaExceeded = status.Used >= status.Limit

		if status.QuotaExceeded {
			status.ErrorCode = "deep_research_quota_exceeded"
			status.ErrorMessage = fmt.Sprintf("You've exceeded your monthly quota (2 uses per month). You can use Deep Research again on %s", nextMonthStart.Format("January 2, 2006"))
		}
	} else {
		// Free users: 1 session lifetime
		status.Limit = 1
		status.Used = totalCompleted
		status.Remaining = status.Limit - status.Used
		if status.Remaining < 0 {
			status.Remaining = 0
		}

		status.QuotaExceeded = status.Used >= status.Limit

		if status.QuotaExceeded {
			status.ErrorCode = "deep_research_quota_exceeded"
			status.ErrorMessage = "You've exceeded the free user quota. Please upgrade to Pro keep using Deep Research."
		}
	}

	return status, nil
}
