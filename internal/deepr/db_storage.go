package deepr

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"time"

	"github.com/eternisai/enchanted-proxy/internal/logger"
	"github.com/google/uuid"
)

// DBStorage handles persistence of deep research messages to PostgreSQL
type DBStorage struct {
	logger *logger.Logger
	db     *sql.DB
}

// NewDBStorage creates a new database storage instance
func NewDBStorage(logger *logger.Logger, db *sql.DB) *DBStorage {
	logger.WithComponent("deepr-db-storage").Info("database storage initialized")

	return &DBStorage{
		logger: logger,
		db:     db,
	}
}

// AddMessage adds a new message to the database
func (s *DBStorage) AddMessage(userID, chatID, message string, sent bool, messageType string) error {
	log := s.logger.WithComponent("deepr-db-storage")

	messageID := uuid.New().String()
	// Use double underscore as separator to match Firestore format
	sessionID := fmt.Sprintf("%s__%s", userID, chatID)
	now := time.Now()

	var sentAt *time.Time
	if sent {
		sentAt = &now
	}

	query := `
		INSERT INTO deep_research_messages (id, user_id, chat_id, session_id, message, message_type, sent, created_at, sent_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
	`

	_, err := s.db.Exec(query, messageID, userID, chatID, sessionID, message, messageType, sent, now, sentAt)
	if err != nil {
		log.Error("failed to add message to database",
			slog.String("user_id", userID),
			slog.String("chat_id", chatID),
			slog.String("message_type", messageType),
			slog.String("error", err.Error()))
		return fmt.Errorf("failed to add message: %w", err)
	}

	log.Debug("message added to database",
		slog.String("user_id", userID),
		slog.String("chat_id", chatID),
		slog.String("message_id", messageID),
		slog.String("message_type", messageType),
		slog.Bool("sent", sent))

	return nil
}

// GetUnsentMessages retrieves all unsent messages for a session
func (s *DBStorage) GetUnsentMessages(userID, chatID string) ([]PersistedMessage, error) {
	log := s.logger.WithComponent("deepr-db-storage")

	// Use double underscore as separator to match Firestore format
	sessionID := fmt.Sprintf("%s__%s", userID, chatID)

	query := `
		SELECT id, user_id, chat_id, message, message_type, sent, created_at
		FROM deep_research_messages
		WHERE session_id = $1 AND sent = FALSE
		ORDER BY created_at ASC
	`

	rows, err := s.db.Query(query, sessionID)
	if err != nil {
		log.Error("failed to query unsent messages",
			slog.String("user_id", userID),
			slog.String("chat_id", chatID),
			slog.String("error", err.Error()))
		return nil, fmt.Errorf("failed to query unsent messages: %w", err)
	}
	defer rows.Close()

	var messages []PersistedMessage
	for rows.Next() {
		var msg PersistedMessage
		err := rows.Scan(&msg.ID, &msg.UserID, &msg.ChatID, &msg.Message, &msg.MessageType, &msg.Sent, &msg.Timestamp)
		if err != nil {
			log.Error("failed to scan message row",
				slog.String("user_id", userID),
				slog.String("chat_id", chatID),
				slog.String("error", err.Error()))
			return nil, fmt.Errorf("failed to scan message: %w", err)
		}
		messages = append(messages, msg)
	}

	if err = rows.Err(); err != nil {
		log.Error("error iterating message rows",
			slog.String("user_id", userID),
			slog.String("chat_id", chatID),
			slog.String("error", err.Error()))
		return nil, fmt.Errorf("error iterating messages: %w", err)
	}

	log.Info("retrieved unsent messages",
		slog.String("user_id", userID),
		slog.String("chat_id", chatID),
		slog.Int("unsent_count", len(messages)))

	return messages, nil
}

// MarkMessageAsSent marks a specific message as sent
func (s *DBStorage) MarkMessageAsSent(userID, chatID, messageID string) error {
	log := s.logger.WithComponent("deepr-db-storage")

	query := `
		UPDATE deep_research_messages
		SET sent = TRUE, sent_at = NOW()
		WHERE id = $1
	`

	result, err := s.db.Exec(query, messageID)
	if err != nil {
		log.Error("failed to mark message as sent",
			slog.String("user_id", userID),
			slog.String("chat_id", chatID),
			slog.String("message_id", messageID),
			slog.String("error", err.Error()))
		return fmt.Errorf("failed to mark message as sent: %w", err)
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		log.Warn("failed to get rows affected",
			slog.String("message_id", messageID),
			slog.String("error", err.Error()))
	} else {
		log.Debug("message marked as sent",
			slog.String("user_id", userID),
			slog.String("chat_id", chatID),
			slog.String("message_id", messageID),
			slog.Int64("rows_affected", rowsAffected))
	}

	return nil
}

// MarkAllMessagesAsSent marks all unsent messages for a session as sent
func (s *DBStorage) MarkAllMessagesAsSent(userID, chatID string) error {
	log := s.logger.WithComponent("deepr-db-storage")

	// Use double underscore as separator to match Firestore format
	sessionID := fmt.Sprintf("%s__%s", userID, chatID)

	query := `
		UPDATE deep_research_messages
		SET sent = TRUE, sent_at = NOW()
		WHERE session_id = $1 AND sent = FALSE
	`

	result, err := s.db.Exec(query, sessionID)
	if err != nil {
		log.Error("failed to mark all messages as sent",
			slog.String("user_id", userID),
			slog.String("chat_id", chatID),
			slog.String("error", err.Error()))
		return fmt.Errorf("failed to mark all messages as sent: %w", err)
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		log.Warn("failed to get rows affected",
			slog.String("user_id", userID),
			slog.String("chat_id", chatID),
			slog.String("error", err.Error()))
	} else {
		log.Info("all messages marked as sent",
			slog.String("user_id", userID),
			slog.String("chat_id", chatID),
			slog.Int64("rows_affected", rowsAffected))
	}

	return nil
}

// UpdateBackendConnectionStatus is a no-op for database storage
// Connection status is tracked via session state in Firebase
func (s *DBStorage) UpdateBackendConnectionStatus(userID, chatID string, connected bool) error {
	// No-op: backend connection status is tracked via Firebase session state
	return nil
}

// IsSessionComplete checks if a session has completed (has research_complete or error message)
func (s *DBStorage) IsSessionComplete(userID, chatID string) (bool, error) {
	log := s.logger.WithComponent("deepr-db-storage")

	// Use double underscore as separator to match Firestore format
	sessionID := fmt.Sprintf("%s__%s", userID, chatID)

	query := `
		SELECT COUNT(*) > 0 as is_complete
		FROM deep_research_messages
		WHERE session_id = $1 AND message_type IN ('research_complete', 'error')
		LIMIT 1
	`

	var isComplete bool
	err := s.db.QueryRow(query, sessionID).Scan(&isComplete)
	if err != nil {
		log.Error("failed to check session completion",
			slog.String("user_id", userID),
			slog.String("chat_id", chatID),
			slog.String("error", err.Error()))
		return false, fmt.Errorf("failed to check session completion: %w", err)
	}

	log.Debug("session completion status checked",
		slog.String("user_id", userID),
		slog.String("chat_id", chatID),
		slog.Bool("is_complete", isComplete))

	return isComplete, nil
}

// CleanupOldSessions removes messages older than the specified duration
func (s *DBStorage) CleanupOldSessions(ctx context.Context, maxAge time.Duration) error {
	log := s.logger.WithComponent("deepr-db-storage")

	cutoffTime := time.Now().Add(-maxAge)

	query := `
		DELETE FROM deep_research_messages
		WHERE created_at < $1
	`

	result, err := s.db.ExecContext(ctx, query, cutoffTime)
	if err != nil {
		log.Error("failed to cleanup old sessions",
			slog.Duration("max_age", maxAge),
			slog.Time("cutoff_time", cutoffTime),
			slog.String("error", err.Error()))
		return fmt.Errorf("failed to cleanup old sessions: %w", err)
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		log.Warn("failed to get rows affected for cleanup",
			slog.String("error", err.Error()))
	} else {
		log.Info("old sessions cleaned up",
			slog.Int64("messages_deleted", rowsAffected),
			slog.Duration("max_age", maxAge))
	}

	return nil
}
