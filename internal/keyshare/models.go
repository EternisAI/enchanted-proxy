package keyshare

import (
	"time"
)

// SessionStatus represents the state of a key sharing session
type SessionStatus string

const (
	SessionStatusPending   SessionStatus = "pending"
	SessionStatusCompleted SessionStatus = "completed"
	SessionStatusExpired   SessionStatus = "expired"
)

// EphemeralPublicKey represents a JWK-formatted ephemeral public key (P-256 curve)
type EphemeralPublicKey struct {
	Kty string `json:"kty" firestore:"kty"` // Key type (must be "EC")
	Crv string `json:"crv" firestore:"crv"` // Curve (must be "P-256")
	X   string `json:"x" firestore:"x"`     // X coordinate (base64url)
	Y   string `json:"y" firestore:"y"`     // Y coordinate (base64url)
}

// KeyShareSession represents a session for sharing encryption keys between devices
type KeyShareSession struct {
	SessionID           string             `json:"sessionId" firestore:"sessionId"`
	UserID              string             `json:"userId" firestore:"userId"`
	EphemeralPublicKey  EphemeralPublicKey `json:"ephemeralPublicKey" firestore:"ephemeralPublicKey"`
	Status              SessionStatus      `json:"status" firestore:"status"`
	EncryptedPrivateKey string             `json:"encryptedPrivateKey,omitempty" firestore:"encryptedPrivateKey,omitempty"`
	CreatedAt           time.Time          `json:"createdAt" firestore:"createdAt"`
	ExpiresAt           time.Time          `json:"expiresAt" firestore:"expiresAt"`
	CompletedAt         *time.Time         `json:"completedAt,omitempty" firestore:"completedAt,omitempty"`
}

// CreateSessionRequest represents the request to create a new key sharing session
type CreateSessionRequest struct {
	EphemeralPublicKey EphemeralPublicKey `json:"ephemeralPublicKey" binding:"required"`
}

// CreateSessionResponse represents the response when creating a session
type CreateSessionResponse struct {
	SessionID string `json:"sessionId"`
	ExpiresAt string `json:"expiresAt"` // ISO 8601 format
}

// SubmitKeyRequest represents the request to submit an encrypted key to a session
type SubmitKeyRequest struct {
	EncryptedPrivateKey string `json:"encryptedPrivateKey" binding:"required"`
}

// SubmitKeyResponse represents the response when submitting a key
type SubmitKeyResponse struct {
	Success bool `json:"success"`
}

// WebSocketMessage represents messages sent over the WebSocket connection
type WebSocketMessage struct {
	Type                string `json:"type"`
	SessionID           string `json:"sessionId,omitempty"`
	EncryptedPrivateKey string `json:"encryptedPrivateKey,omitempty"`
	Message             string `json:"message,omitempty"`
	Error               string `json:"error,omitempty"`
}

// WebSocket message types
const (
	WSMessageTypeConnected      = "connected"
	WSMessageTypeKeyReceived    = "key_received"
	WSMessageTypeSessionExpired = "session_expired"
	WSMessageTypeError          = "error"
)

// ErrorResponse represents an API error response
type ErrorResponse struct {
	Error   string `json:"error"`
	Message string `json:"message"`
}
