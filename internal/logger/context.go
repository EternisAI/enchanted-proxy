package logger

import (
	"context"
	"crypto/rand"
	"encoding/hex"
)

// WithRequestID adds a request ID to the context.
func WithRequestID(ctx context.Context, requestID string) context.Context {
	return context.WithValue(ctx, ContextKeyRequestID, requestID)
}

// WithUserID adds a user ID to the context.
func WithUserID(ctx context.Context, userID string) context.Context {
	return context.WithValue(ctx, ContextKeyUserID, userID)
}

// WithOperation adds an operation name to the context.
func WithOperation(ctx context.Context, operation string) context.Context {
	return context.WithValue(ctx, ContextKeyOperation, operation)
}

// GenerateRequestID generates a new request ID.
func GenerateRequestID() string {
	bytes := make([]byte, 8)
	rand.Read(bytes) //nolint:errcheck
	return hex.EncodeToString(bytes)
}
