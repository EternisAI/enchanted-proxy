package keyshare

import (
	"context"
	"time"

	"cloud.google.com/go/firestore"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	// CollectionName is the Firestore collection for key sharing sessions
	CollectionName = "keyShareSessions"
)

// FirestoreClient handles Firestore operations for key sharing sessions
type FirestoreClient struct {
	client *firestore.Client
}

// NewFirestoreClient creates a new Firestore client wrapper
func NewFirestoreClient(client *firestore.Client) *FirestoreClient {
	if client == nil {
		return nil
	}
	return &FirestoreClient{client: client}
}

// CreateSession creates a new key sharing session in Firestore
func (f *FirestoreClient) CreateSession(ctx context.Context, session *KeyShareSession) error {
	if f == nil || f.client == nil {
		return status.Error(codes.Internal, "firestore client is nil")
	}
	if session == nil || session.SessionID == "" {
		return status.Error(codes.InvalidArgument, "session and sessionID must be non-empty")
	}

	docRef := f.client.Collection(CollectionName).Doc(session.SessionID)
	_, err := docRef.Create(ctx, session)
	if err != nil {
		if status.Code(err) == codes.AlreadyExists {
			return status.Error(codes.AlreadyExists, "session already exists")
		}
		return status.Errorf(codes.Internal, "failed to create session: %v", err)
	}

	return nil
}

// GetSession retrieves a session by ID
func (f *FirestoreClient) GetSession(ctx context.Context, sessionID string) (*KeyShareSession, error) {
	if f == nil || f.client == nil {
		return nil, status.Error(codes.Internal, "firestore client is nil")
	}
	if sessionID == "" {
		return nil, status.Error(codes.InvalidArgument, "sessionID must be non-empty")
	}

	docRef := f.client.Collection(CollectionName).Doc(sessionID)
	doc, err := docRef.Get(ctx)
	if err != nil {
		if status.Code(err) == codes.NotFound {
			return nil, status.Errorf(codes.NotFound, "session not found: %s", sessionID)
		}
		return nil, status.Errorf(codes.Internal, "failed to get session: %v", err)
	}

	var session KeyShareSession
	if err := doc.DataTo(&session); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to parse session: %v", err)
	}

	return &session, nil
}

// UpdateSessionWithKey updates a session with the encrypted private key and marks it as completed
func (f *FirestoreClient) UpdateSessionWithKey(ctx context.Context, sessionID, encryptedPrivateKey string) error {
	if f == nil || f.client == nil {
		return status.Error(codes.Internal, "firestore client is nil")
	}
	if sessionID == "" || encryptedPrivateKey == "" {
		return status.Error(codes.InvalidArgument, "sessionID and encryptedPrivateKey must be non-empty")
	}

	docRef := f.client.Collection(CollectionName).Doc(sessionID)
	now := time.Now()

	_, err := docRef.Update(ctx, []firestore.Update{
		{Path: "status", Value: SessionStatusCompleted},
		{Path: "encryptedPrivateKey", Value: encryptedPrivateKey},
		{Path: "completedAt", Value: now},
	})
	if err != nil {
		if status.Code(err) == codes.NotFound {
			return status.Error(codes.NotFound, "session not found")
		}
		return status.Errorf(codes.Internal, "failed to update session: %v", err)
	}

	return nil
}

// CountRecentSessions counts sessions created by a user in the last hour (for rate limiting)
func (f *FirestoreClient) CountRecentSessions(ctx context.Context, userID string) (int64, error) {
	if f == nil || f.client == nil {
		return 0, status.Error(codes.Internal, "firestore client is nil")
	}
	if userID == "" {
		return 0, status.Error(codes.InvalidArgument, "userID must be non-empty")
	}

	oneHourAgo := time.Now().Add(-1 * time.Hour)

	query := f.client.Collection(CollectionName).
		Where("userId", "==", userID).
		Where("createdAt", ">", oneHourAgo)

	snapshot, err := query.Documents(ctx).GetAll()
	if err != nil {
		return 0, status.Errorf(codes.Internal, "failed to count sessions: %v", err)
	}

	return int64(len(snapshot)), nil
}

// DeleteExpiredSessions deletes sessions that have expired (for cleanup job)
func (f *FirestoreClient) DeleteExpiredSessions(ctx context.Context, batchSize int) (int, error) {
	if f == nil || f.client == nil {
		return 0, status.Error(codes.Internal, "firestore client is nil")
	}

	now := time.Now()

	// Find expired sessions
	query := f.client.Collection(CollectionName).
		Where("expiresAt", "<", now).
		Limit(batchSize)

	snapshot, err := query.Documents(ctx).GetAll()
	if err != nil {
		return 0, status.Errorf(codes.Internal, "failed to query expired sessions: %v", err)
	}

	if len(snapshot) == 0 {
		return 0, nil
	}

	// Delete in batch
	batch := f.client.Batch()
	for _, doc := range snapshot {
		batch.Delete(doc.Ref)
	}

	_, err = batch.Commit(ctx)
	if err != nil {
		return 0, status.Errorf(codes.Internal, "failed to delete expired sessions: %v", err)
	}

	return len(snapshot), nil
}
