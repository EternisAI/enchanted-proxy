package deepr

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/eternisai/enchanted-proxy/internal/logger"
	"github.com/google/uuid"
)

// Storage handles persistence of deep research messages
type Storage struct {
	logger      *logger.Logger
	storagePath string
	mu          sync.RWMutex
}

// NewStorage creates a new storage instance
func NewStorage(logger *logger.Logger, storagePath string) (*Storage, error) {
	// Create storage directory if it doesn't exist
	if err := os.MkdirAll(storagePath, 0755); err != nil {
		logger.WithComponent("deepr-storage").Error("failed to create storage directory",
			slog.String("path", storagePath),
			slog.String("error", err.Error()))
		return nil, fmt.Errorf("failed to create storage directory: %w", err)
	}

	logger.WithComponent("deepr-storage").Info("storage initialized",
		slog.String("path", storagePath))

	return &Storage{
		logger:      logger,
		storagePath: storagePath,
	}, nil
}

// getSessionFilePath returns the file path for a session
func (s *Storage) getSessionFilePath(userID, chatID string) string {
	filename := fmt.Sprintf("session_%s_%s.json", userID, chatID)
	return filepath.Join(s.storagePath, filename)
}

// LoadSession loads a session state from disk
func (s *Storage) LoadSession(userID, chatID string) (*SessionState, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	filePath := s.getSessionFilePath(userID, chatID)

	data, err := os.ReadFile(filePath)
	if err != nil {
		if os.IsNotExist(err) {
			// No existing session, return new one
			return &SessionState{
				UserID:              userID,
				ChatID:              chatID,
				Messages:            []PersistedMessage{},
				BackendConnected:    false,
				LastActivity:        time.Now(),
				FinalReportReceived: false,
				ErrorOccurred:       false,
			}, nil
		}
		return nil, fmt.Errorf("failed to read session file: %w", err)
	}

	var state SessionState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("failed to unmarshal session: %w", err)
	}

	return &state, nil
}

// SaveSession saves a session state to disk
func (s *Storage) SaveSession(state *SessionState) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.saveSessionUnsafe(state)
}

// loadSessionUnsafe loads a session without acquiring locks (internal use only)
func (s *Storage) loadSessionUnsafe(userID, chatID string) (*SessionState, error) {
	filePath := s.getSessionFilePath(userID, chatID)

	data, err := os.ReadFile(filePath)
	if err != nil {
		if os.IsNotExist(err) {
			// No existing session, return new one
			return &SessionState{
				UserID:              userID,
				ChatID:              chatID,
				Messages:            []PersistedMessage{},
				BackendConnected:    false,
				LastActivity:        time.Now(),
				FinalReportReceived: false,
				ErrorOccurred:       false,
			}, nil
		}
		return nil, fmt.Errorf("failed to read session file: %w", err)
	}

	var state SessionState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("failed to unmarshal session: %w", err)
	}

	return &state, nil
}

// saveSessionUnsafe saves a session without acquiring locks (internal use only)
func (s *Storage) saveSessionUnsafe(state *SessionState) error {
	filePath := s.getSessionFilePath(state.UserID, state.ChatID)

	state.LastActivity = time.Now()

	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal session: %w", err)
	}

	if err := os.WriteFile(filePath, data, 0644); err != nil {
		return fmt.Errorf("failed to write session file: %w", err)
	}

	return nil
}

// modifySession executes a mutation function on a session while holding the write lock
func (s *Storage) modifySession(userID, chatID string, mutate func(*SessionState) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	state, err := s.loadSessionUnsafe(userID, chatID)
	if err != nil {
		return err
	}

	if err := mutate(state); err != nil {
		return err
	}

	return s.saveSessionUnsafe(state)
}

// AddMessage adds a new message to the session
func (s *Storage) AddMessage(userID, chatID, message string, sent bool, messageType string) error {
	err := s.modifySession(userID, chatID, func(state *SessionState) error {
		persistedMsg := PersistedMessage{
			ID:          uuid.New().String(),
			UserID:      userID,
			ChatID:      chatID,
			Message:     message,
			Sent:        sent,
			Timestamp:   time.Now(),
			MessageType: messageType,
		}

		state.Messages = append(state.Messages, persistedMsg)

		// Check if this is a final report or error
		var msg Message
		if err := json.Unmarshal([]byte(message), &msg); err == nil {
			if msg.FinalReport != "" {
				state.FinalReportReceived = true
			}
			if msg.Type == "error" || msg.Error != "" {
				state.ErrorOccurred = true
			}
		}

		s.logger.WithComponent("deepr-storage").Debug("message added to session",
			slog.String("user_id", userID),
			slog.String("chat_id", chatID),
			slog.String("message_id", persistedMsg.ID),
			slog.String("message_type", messageType),
			slog.Bool("sent", sent),
			slog.Int("total_messages", len(state.Messages)))

		return nil
	})

	if err != nil {
		s.logger.WithComponent("deepr-storage").Error("failed to add message",
			slog.String("user_id", userID),
			slog.String("chat_id", chatID),
			slog.String("message_type", messageType),
			slog.String("error", err.Error()))
	}

	return err
}

// MarkMessageAsSent marks a specific message as sent
func (s *Storage) MarkMessageAsSent(userID, chatID, messageID string) error {
	return s.modifySession(userID, chatID, func(state *SessionState) error {
		for i := range state.Messages {
			if state.Messages[i].ID == messageID {
				state.Messages[i].Sent = true
				break
			}
		}
		return nil
	})
}

// MarkAllMessagesAsSent marks all messages up to a certain index as sent
func (s *Storage) MarkAllMessagesAsSent(userID, chatID string) error {
	return s.modifySession(userID, chatID, func(state *SessionState) error {
		for i := range state.Messages {
			state.Messages[i].Sent = true
		}
		return nil
	})
}

// GetUnsentMessages returns all unsent messages for a session
func (s *Storage) GetUnsentMessages(userID, chatID string) ([]PersistedMessage, error) {
	state, err := s.LoadSession(userID, chatID)
	if err != nil {
		s.logger.WithComponent("deepr-storage").Error("failed to load session for unsent messages",
			slog.String("user_id", userID),
			slog.String("chat_id", chatID),
			slog.String("error", err.Error()))
		return nil, err
	}

	var unsent []PersistedMessage
	for _, msg := range state.Messages {
		if !msg.Sent {
			unsent = append(unsent, msg)
		}
	}

	if len(unsent) > 0 {
		s.logger.WithComponent("deepr-storage").Info("retrieved unsent messages",
			slog.String("user_id", userID),
			slog.String("chat_id", chatID),
			slog.Int("unsent_count", len(unsent)),
			slog.Int("total_messages", len(state.Messages)))
	}

	return unsent, nil
}

// GetLastUnsentMessage returns the last unsent message for a session
func (s *Storage) GetLastUnsentMessage(userID, chatID string) (*PersistedMessage, error) {
	unsent, err := s.GetUnsentMessages(userID, chatID)
	if err != nil {
		return nil, err
	}

	if len(unsent) == 0 {
		return nil, nil
	}

	return &unsent[len(unsent)-1], nil
}

// UpdateBackendConnectionStatus updates the backend connection status
func (s *Storage) UpdateBackendConnectionStatus(userID, chatID string, connected bool) error {
	err := s.modifySession(userID, chatID, func(state *SessionState) error {
		state.BackendConnected = connected
		return nil
	})

	if err == nil {
		s.logger.WithComponent("deepr-storage").Info("backend connection status updated",
			slog.String("user_id", userID),
			slog.String("chat_id", chatID),
			slog.Bool("connected", connected))
	} else {
		s.logger.WithComponent("deepr-storage").Error("failed to update backend connection status",
			slog.String("user_id", userID),
			slog.String("chat_id", chatID),
			slog.Bool("connected", connected),
			slog.String("error", err.Error()))
	}

	return err
}

// IsSessionComplete checks if a session is complete (has final report or error)
func (s *Storage) IsSessionComplete(userID, chatID string) (bool, error) {
	state, err := s.LoadSession(userID, chatID)
	if err != nil {
		s.logger.WithComponent("deepr-storage").Error("failed to load session for completion check",
			slog.String("user_id", userID),
			slog.String("chat_id", chatID),
			slog.String("error", err.Error()))
		return false, err
	}

	isComplete := state.FinalReportReceived || state.ErrorOccurred

	s.logger.WithComponent("deepr-storage").Debug("session completion status checked",
		slog.String("user_id", userID),
		slog.String("chat_id", chatID),
		slog.Bool("is_complete", isComplete),
		slog.Bool("has_final_report", state.FinalReportReceived),
		slog.Bool("has_error", state.ErrorOccurred))

	return isComplete, nil
}

// CleanupOldSessions removes session files older than the specified duration
func (s *Storage) CleanupOldSessions(maxAge time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.logger.WithComponent("deepr-storage").Info("starting session cleanup",
		slog.Duration("max_age", maxAge))

	files, err := os.ReadDir(s.storagePath)
	if err != nil {
		s.logger.WithComponent("deepr-storage").Error("failed to read storage directory for cleanup",
			slog.String("path", s.storagePath),
			slog.String("error", err.Error()))
		return fmt.Errorf("failed to read storage directory: %w", err)
	}

	now := time.Now()
	removedCount := 0
	errorCount := 0
	totalFiles := 0

	for _, file := range files {
		if file.IsDir() {
			continue
		}

		totalFiles++
		filePath := filepath.Join(s.storagePath, file.Name())
		info, err := file.Info()
		if err != nil {
			s.logger.WithComponent("deepr-storage").Error("failed to get file info during cleanup",
				slog.String("file", file.Name()),
				slog.String("error", err.Error()))
			errorCount++
			continue
		}

		fileAge := now.Sub(info.ModTime())
		if fileAge > maxAge {
			if err := os.Remove(filePath); err != nil {
				s.logger.WithComponent("deepr-storage").Error("failed to remove old session file",
					slog.String("file", file.Name()),
					slog.Duration("age", fileAge),
					slog.String("error", err.Error()))
				errorCount++
			} else {
				s.logger.WithComponent("deepr-storage").Info("old session file removed",
					slog.String("file", file.Name()),
					slog.Duration("age", fileAge))
				removedCount++
			}
		}
	}

	s.logger.WithComponent("deepr-storage").Info("session cleanup completed",
		slog.Int("total_files", totalFiles),
		slog.Int("removed_count", removedCount),
		slog.Int("error_count", errorCount),
		slog.Duration("max_age", maxAge))

	return nil
}
