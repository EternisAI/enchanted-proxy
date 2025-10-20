package messaging

import (
	"context"
	"fmt"

	"cloud.google.com/go/firestore"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// FirestoreClient handles Firestore operations for messages
type FirestoreClient struct {
	client *firestore.Client
}

// NewFirestoreClient creates a new Firestore client wrapper
func NewFirestoreClient(client *firestore.Client) *FirestoreClient {
	return &FirestoreClient{client: client}
}

// GetUserPublicKey retrieves a user's public key
// Path: /users/{userId}/accountKey
func (f *FirestoreClient) GetUserPublicKey(ctx context.Context, userID string) (*UserPublicKey, error) {
	// Get documents from accountKey subcollection
	docs, err := f.client.Collection("users").Doc(userID).Collection("accountKey").Documents(ctx).GetAll()
	if err != nil {
		return nil, fmt.Errorf("failed to query public key: %w", err)
	}

	if len(docs) == 0 {
		return nil, fmt.Errorf("no public key found for user %s", userID)
	}

	var key UserPublicKey
	if err := docs[0].DataTo(&key); err != nil {
		return nil, fmt.Errorf("failed to parse public key: %w", err)
	}

	return &key, nil
}

// SaveMessage saves an encrypted message to Firestore
// Path: /chats/{userId}/{chatId}/messages/{messageId}
func (f *FirestoreClient) SaveMessage(ctx context.Context, userID string, msg *ChatMessage) error {
	docRef := f.client.
		Collection("chats").
		Doc(userID).
		Collection(msg.ChatID).
		Doc(msg.ID)

	_, err := docRef.Set(ctx, msg)
	if err != nil {
		return fmt.Errorf("failed to save message: %w", err)
	}

	return nil
}

// GetMessage retrieves a message from Firestore
func (f *FirestoreClient) GetMessage(ctx context.Context, userID, chatID, messageID string) (*ChatMessage, error) {
	docRef := f.client.
		Collection("chats").
		Doc(userID).
		Collection(chatID).
		Doc(messageID)

	doc, err := docRef.Get(ctx)
	if err != nil {
		if status.Code(err) == codes.NotFound {
			return nil, fmt.Errorf("message not found")
		}
		return nil, fmt.Errorf("failed to get message: %w", err)
	}

	var msg ChatMessage
	if err := doc.DataTo(&msg); err != nil {
		return nil, fmt.Errorf("failed to parse message: %w", err)
	}

	return &msg, nil
}
