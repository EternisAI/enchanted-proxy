package messaging

import "time"

// ChatMessage represents a stored chat message in Firestore
type ChatMessage struct {
	ID                  string    `firestore:"id"`                  // Message UUID
	EncryptedContent    string    `firestore:"encryptedContent"`    // Encrypted message content
	IsFromUser          bool      `firestore:"isFromUser"`          // true = user, false = assistant
	ChatID              string    `firestore:"chatId"`              // Chat UUID
	IsError             bool      `firestore:"isError"`             // true if error occurred
	Timestamp           time.Time `firestore:"timestamp"`           // Message timestamp
	PublicEncryptionKey string    `firestore:"publicEncryptionKey"` // Public key used (JSON string or "none")

	// Stop control fields (for AI responses that were stopped mid-generation)
	Stopped    bool   `firestore:"stopped,omitempty"`    // true if generation was stopped by user/system
	StoppedBy  string `firestore:"stoppedBy,omitempty"`  // User ID who stopped, or "system_timeout"/"system_shutdown"
	StopReason string `firestore:"stopReason,omitempty"` // Why stopped: "user_cancelled", "timeout", "error", "system_shutdown"
}

// UserPublicKey represents a user's ECDSA P-256 public key
type UserPublicKey struct {
	CreatedAt time.Time `firestore:"createdAt"`
	Public    string    `firestore:"public"` // JWK JSON string (EC P-256)
	UpdatedAt time.Time `firestore:"updatedAt"`
	Version   int       `firestore:"version"` // Key version number
}

// JWKPublicKey represents the parsed JWK public key
type JWKPublicKey struct {
	Crv    string   `json:"crv"`     // "P-256"
	Ext    bool     `json:"ext"`     // true
	KeyOps []string `json:"key_ops"` // []
	Kty    string   `json:"kty"`     // "EC"
	X      string   `json:"x"`       // Base64url-encoded X coordinate
	Y      string   `json:"y"`       // Base64url-encoded Y coordinate
}

// MessageToStore is the internal representation for messages to be stored
type MessageToStore struct {
	UserID            string
	ChatID            string
	MessageID         string
	IsFromUser        bool
	Content           string // Plaintext content to be encrypted
	IsError           bool
	EncryptionEnabled *bool // nil = not specified (backward compat), true = enforce encryption, false = store plaintext

	// Stop control (for streaming broadcast feature)
	Stopped    bool   // true if generation was stopped mid-stream
	StoppedBy  string // User ID who stopped, or "system_timeout"/"system_shutdown"
	StopReason string // Why stopped: "user_cancelled", "timeout", "error", "system_shutdown"
}

// ChatTitle represents a stored chat title in Firestore
// IMPORTANT: Only ONE of Title or EncryptedTitle should be set, never both
type ChatTitle struct {
	Title                    string    `firestore:"title,omitempty"`                    // Plaintext title (only when encryption disabled)
	EncryptedTitle           string    `firestore:"encryptedTitle,omitempty"`           // Encrypted title (only when encryption enabled)
	TitlePublicEncryptionKey string    `firestore:"titlePublicEncryptionKey,omitempty"` // Public key used (only when encrypted)
	UpdatedAt                time.Time `firestore:"updatedAt"`                          // Last update timestamp
}
