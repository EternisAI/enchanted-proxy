package deepr

// MessageStorage defines the interface for storing deep research messages
// Implementations: DBStorage (database-backed, recommended).
type MessageStorage interface {
	AddMessage(userID, chatID, message string, sent bool, messageType string) error
	GetUnsentMessages(userID, chatID string) ([]PersistedMessage, error)
	MarkMessageAsSent(userID, chatID, messageID string) error
	MarkAllMessagesAsSent(userID, chatID string) error
	UpdateBackendConnectionStatus(userID, chatID string, connected bool) error
	IsSessionComplete(userID, chatID string) (bool, error)
}
