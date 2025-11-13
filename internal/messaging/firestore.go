package messaging

import (
	"context"
	"time"

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
// Path: /users/{userId} -> accountKey field
func (f *FirestoreClient) GetUserPublicKey(ctx context.Context, userID string) (*UserPublicKey, error) {
	if f == nil || f.client == nil {
		return nil, status.Error(codes.Internal, "firestore client is nil")
	}
	if userID == "" {
		return nil, status.Error(codes.InvalidArgument, "userID must be non-empty")
	}

	// Get user document
	docRef := f.client.Collection("users").Doc(userID)
	doc, err := docRef.Get(ctx)
	if err != nil {
		if status.Code(err) == codes.NotFound {
			return nil, status.Errorf(codes.NotFound, "user document not found for user %s", userID)
		}
		return nil, status.Errorf(codes.Internal, "failed to get user document for user %s: %v", userID, err)
	}

	// Extract accountKey field
	accountKeyData, err := doc.DataAt("accountKey")
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "no public key found for user %s", userID)
	}

	// Parse accountKey map into UserPublicKey struct
	accountKeyMap, ok := accountKeyData.(map[string]interface{})
	if !ok {
		return nil, status.Errorf(codes.Internal, "accountKey field is not a map for user %s", userID)
	}

	var key UserPublicKey
	// Map fields from the accountKey map
	if createdAt, ok := accountKeyMap["createdAt"].(time.Time); ok {
		key.CreatedAt = createdAt
	}
	if public, ok := accountKeyMap["public"].(string); ok {
		key.Public = public
	}
	if updatedAt, ok := accountKeyMap["updatedAt"].(time.Time); ok {
		key.UpdatedAt = updatedAt
	}
	if version, ok := accountKeyMap["version"].(int64); ok {
		key.Version = int(version)
	}

	// Validate that public key exists
	if key.Public == "" {
		return nil, status.Errorf(codes.NotFound, "public key is empty for user %s", userID)
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

	// Update parent chat document with lastMessageAt timestamp (if it exists)
	// IMPORTANT: We use Update() not Set() to avoid creating the chat document
	// Chat document creation is the client's responsibility
	chatDocRef := f.client.
		Collection("users").
		Doc(userID).
		Collection("chats").
		Doc(msg.ChatID)

	// Update (not create) chat document with lastMessageAt timestamp
	// If chat document doesn't exist, this will fail - which is expected
	// The client should create the chat document before sending messages
	_, err := chatDocRef.Update(ctx, []firestore.Update{
		{Path: "lastMessageAt", Value: msg.Timestamp},
		{Path: "updatedAt", Value: msg.Timestamp},
	})
	if err != nil {
		// If chat document doesn't exist, log warning but continue with message save
		// This allows graceful degradation if client forgets to create chat doc
		if status.Code(err) == codes.NotFound {
			// Don't fail - just log warning and continue
			// Message will still be saved, but chat doc won't be updated
			// Client will create chat doc when it's ready
		} else {
			return status.Errorf(codes.Internal, "failed to update chat document user=%s chat=%s: %v", userID, msg.ChatID, err)
		}
	}

	// Path: /users/{userId}/chats/{chatId}/messages/{messageId}
	docRef := f.client.
		Collection("users").
		Doc(userID).
		Collection("chats").
		Doc(msg.ChatID).
		Collection("messages").
		Doc(msg.ID)

	// Use Create for idempotency - treat AlreadyExists as success
	_, err = docRef.Create(ctx, msg)
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

	// Path: /users/{userId}/chats/{chatId}/messages/{messageId}
	docRef := f.client.
		Collection("users").
		Doc(userID).
		Collection("chats").
		Doc(chatID).
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

// SaveChatTitle saves/updates chat title (plaintext or encrypted)
// Path: /users/{userId}/chats/{chatId}
// IMPORTANT: This only UPDATES existing chat documents, does not create new ones
// IMPORTANT: Only ONE of Title or EncryptedTitle should be set, never both
func (f *FirestoreClient) SaveChatTitle(ctx context.Context, userID, chatID string, title *ChatTitle) error {
	if f == nil || f.client == nil {
		return status.Error(codes.Internal, "firestore client is nil")
	}
	if userID == "" || chatID == "" || title == nil {
		return status.Error(codes.InvalidArgument, "userID, chatID, and title must be non-empty")
	}

	// Validate: exactly one of Title or EncryptedTitle must be set
	hasPlaintext := len(title.Title) > 0
	hasEncrypted := len(title.EncryptedTitle) > 0

	if !hasPlaintext && !hasEncrypted {
		return status.Error(codes.InvalidArgument, "either title or encryptedTitle must be set")
	}
	if hasPlaintext && hasEncrypted {
		return status.Error(codes.InvalidArgument, "cannot set both title and encryptedTitle")
	}

	// Update chat document with title fields
	// IMPORTANT: Use Update() not Set() to avoid creating the chat document
	// Chat document must already exist (created by client)
	docRef := f.client.Collection("users").Doc(userID).Collection("chats").Doc(chatID)

	// Build update list based on whether title is encrypted or plaintext
	updates := []firestore.Update{
		{Path: "updatedAt", Value: title.UpdatedAt},
	}

	if hasEncrypted {
		// Encrypted title: set encryptedTitle and titlePublicEncryptionKey
		updates = append(updates,
			firestore.Update{Path: "encryptedTitle", Value: title.EncryptedTitle},
			firestore.Update{Path: "titlePublicEncryptionKey", Value: title.TitlePublicEncryptionKey},
		)
		// Clear plaintext title if it exists (migration case)
		updates = append(updates, firestore.Update{Path: "title", Value: firestore.Delete})
	} else {
		// Plaintext title: set title only
		updates = append(updates,
			firestore.Update{Path: "title", Value: title.Title},
		)
		// Clear encrypted fields if they exist (migration case)
		updates = append(updates,
			firestore.Update{Path: "encryptedTitle", Value: firestore.Delete},
			firestore.Update{Path: "titlePublicEncryptionKey", Value: firestore.Delete},
		)
	}

	_, err := docRef.Update(ctx, updates)

	if err != nil {
		// If chat document doesn't exist, this is expected - client hasn't created it yet
		// Log as info (not error) and return nil for graceful handling
		if status.Code(err) == codes.NotFound {
			// Chat document doesn't exist yet - client will create it later
			// Title will need to be set again or client will generate it
			return status.Errorf(codes.FailedPrecondition, "chat document not found - client must create chat before title can be saved user=%s chat=%s", userID, chatID)
		}
		return status.Errorf(codes.Internal, "failed to save title user=%s chat=%s: %v", userID, chatID, err)
	}

	return nil
}

// VerifyChatOwnership checks if a user owns a specific chat
// Returns nil if user owns the chat, error otherwise
func (f *FirestoreClient) VerifyChatOwnership(ctx context.Context, userID, chatID string) error {
	if f == nil || f.client == nil {
		return status.Error(codes.Internal, "firestore client is nil")
	}
	if userID == "" || chatID == "" {
		return status.Error(codes.InvalidArgument, "userID and chatID must be non-empty")
	}

	// Path: /users/{userId}/chats/{chatId}
	docRef := f.client.
		Collection("users").
		Doc(userID).
		Collection("chats").
		Doc(chatID)

	// Check if chat document exists
	_, err := docRef.Get(ctx)
	if err != nil {
		if status.Code(err) == codes.NotFound {
			return status.Errorf(codes.PermissionDenied, "chat not found or access denied")
		}
		return status.Errorf(codes.Internal, "failed to verify chat ownership: %v", err)
	}

	return nil
}
