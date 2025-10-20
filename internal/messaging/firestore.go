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
	if client == nil {
		return nil
	}
	return &FirestoreClient{client: client}
}

// GetUserPublicKey retrieves a user's public key
// Path: /users/{userId}/accountKey
func (f *FirestoreClient) GetUserPublicKey(ctx context.Context, userID string) (*UserPublicKey, error) {
	if f == nil || f.client == nil {
		return nil, status.Error(codes.Internal, "firestore client is nil")
	}
	if userID == "" {
		return nil, status.Error(codes.InvalidArgument, "userID must be non-empty")
	}

	// Get documents from accountKey subcollection
	docs, err := f.client.Collection("users").Doc(userID).Collection("accountKey").Documents(ctx).GetAll()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to query public key for user %s: %v", userID, err)
	}

	if len(docs) == 0 {
		return nil, status.Errorf(codes.NotFound, "no public key found for user %s", userID)
	}

	var key UserPublicKey
	if err := docs[0].DataTo(&key); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to parse public key for user %s: %v", userID, err)
	}

	return &key, nil
}

// SaveMessage saves an encrypted message to Firestore
// Path: /chats/{userId}/{chatId}/messages/{messageId}
func (f *FirestoreClient) SaveMessage(ctx context.Context, userID string, msg *ChatMessage) error {
	if f == nil || f.client == nil {
		return status.Error(codes.Internal, "firestore client is nil")
	}
	if userID == "" || msg == nil || msg.ChatID == "" || msg.ID == "" {
		return status.Error(codes.InvalidArgument, "userID, chatID, and messageID must be non-empty")
	}
	// NOTE: EncryptedContent can be either base64 encrypted data OR plaintext (when publicEncryptionKey = "none")
	// This validation only checks non-empty - encryption verification happens at service layer
	if len(msg.EncryptedContent) == 0 {
		return status.Error(codes.InvalidArgument, "encrypted content must be non-empty")
	}

	// Use correct path with "messages" subcollection
	docRef := f.client.
		Collection("chats").
		Doc(userID).
		Collection(msg.ChatID).
		Collection("messages").
		Doc(msg.ID)

	// Use Create for idempotency - treat AlreadyExists as success
	_, err := docRef.Create(ctx, msg)
	if err != nil {
		if status.Code(err) == codes.AlreadyExists {
			return nil // Idempotent - already saved
		}
		return status.Errorf(codes.Internal, "failed to save message user=%s chat=%s id=%s: %v", userID, msg.ChatID, msg.ID, err)
	}

	return nil
}

// GetMessage retrieves a message from Firestore
func (f *FirestoreClient) GetMessage(ctx context.Context, userID, chatID, messageID string) (*ChatMessage, error) {
	if f == nil || f.client == nil {
		return nil, status.Error(codes.Internal, "firestore client is nil")
	}
	if userID == "" || chatID == "" || messageID == "" {
		return nil, status.Error(codes.InvalidArgument, "userID, chatID, and messageID must be non-empty")
	}

	// Use correct path with "messages" subcollection
	docRef := f.client.
		Collection("chats").
		Doc(userID).
		Collection(chatID).
		Collection("messages").
		Doc(messageID)

	doc, err := docRef.Get(ctx)
	if err != nil {
		if status.Code(err) == codes.NotFound {
			return nil, status.Errorf(codes.NotFound, "message not found: user=%s chat=%s id=%s", userID, chatID, messageID)
		}
		return nil, status.Errorf(codes.Internal, "failed to get message user=%s chat=%s id=%s: %v", userID, chatID, messageID, err)
	}

	var msg ChatMessage
	if err := doc.DataTo(&msg); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to parse message user=%s chat=%s id=%s: %v", userID, chatID, messageID, err)
	}

	return &msg, nil
}
