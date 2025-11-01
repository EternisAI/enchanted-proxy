package messaging

import "time"

// ChatMessage represents a stored chat message in Firestore
type ChatMessage struct {
	ID                  string    `firestore:"id"`                   // Message UUID
	EncryptedContent    string    `firestore:"encryptedContent"`     // Encrypted message content
	IsFromUser          bool      `firestore:"isFromUser"`           // true = user, false = assistant
	ChatID              string    `firestore:"chatId"`               // Chat UUID
	IsError             bool      `firestore:"isError"`              // true if error occurred
	Timestamp           time.Time `firestore:"timestamp"`            // Message timestamp
	PublicEncryptionKey string    `firestore:"publicEncryptionKey"` // Public key used (JSON string or "none")
	PasskeyID           string    `firestore:"passkeyId,omitempty"`  // Passkey credential ID used for encryption (optional for backwards compatibility)
}

// UserPublicKey represents a user's ECDSA P-256 public key
type UserPublicKey struct {
	CreatedAt        time.Time `firestore:"createdAt"`
	EncryptedPrivate string    `firestore:"encryptedPrivate"`        // Encrypted private key (not used by proxy)
	Nonce            string    `firestore:"nonce"`                    // Encryption nonce
	PrfSalt          string    `firestore:"prfSalt"`                  // PRF salt for key derivation
	CredentialID     string    `firestore:"credentialId,omitempty"`   // Passkey credential ID (base64, optional)
	Provider         string    `firestore:"provider"`                 // "apple", "android", etc.
	Public           string    `firestore:"public"`                   // JWK JSON string (EC P-256)
	UpdatedAt        time.Time `firestore:"updatedAt"`
	Version          int       `firestore:"version"` // Key version number
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
	UserID     string
	ChatID     string
	MessageID  string
	IsFromUser bool
	Content    string // Plaintext content to be encrypted
	IsError    bool
}

// ChatTitle represents a stored chat title in Firestore
type ChatTitle struct {
	EncryptedTitle           string    `firestore:"encryptedTitle"`           // Encrypted title content or plaintext
	TitlePublicEncryptionKey string    `firestore:"titlePublicEncryptionKey"` // Public key used (JSON string or "none")
	UpdatedAt                time.Time `firestore:"updatedAt"`                // Last update timestamp
}
