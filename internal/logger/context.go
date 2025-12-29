package logger

import (
	"context"

	"github.com/google/uuid"
)

// WithRequestID adds a request ID to the context.
func WithRequestID(ctx context.Context, requestID string) context.Context {
	return context.WithValue(ctx, ContextKeyRequestID, requestID)
}

// WithUserID adds a user ID to the context.
func WithUserID(ctx context.Context, userID string) context.Context {
	return context.WithValue(ctx, ContextKeyUserID, userID)
}

// WithChatID adds a chat ID to the context.
func WithChatID(ctx context.Context, chatID string) context.Context {
	return context.WithValue(ctx, ContextKeyChatID, chatID)
}

// WithOperation adds an operation name to the context.
func WithOperation(ctx context.Context, operation string) context.Context {
	return context.WithValue(ctx, ContextKeyOperation, operation)
}

// GenerateRequestID generates a new request ID.
func GenerateRequestID() string {
	requestID := uuid.New()
	return requestID.String()
}
