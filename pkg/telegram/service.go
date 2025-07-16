package telegram

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/charmbracelet/log"
	pgdb "github.com/eternisai/enchanted-proxy/pkg/storage/pg/sqlc"
	"github.com/nats-io/nats.go"
)

var ErrSubscriptionNilTextMessage = errors.New("subscription stopped due to nil text message")

// Service handles Telegram bot operations.
type Service struct {
	Logger       *log.Logger
	Token        string
	Client       *http.Client
	LastMessages []Message
	NatsClient   *nats.Conn
	queries      pgdb.Querier

	// Message callbacks for direct notification when NATS is not available
	messageCallbacks map[string][]func(Message, string) // chatUUID -> callbacks
	callbacksMu      sync.RWMutex
}

// NewService creates a new Telegram service instance.
func NewService(input TelegramServiceInput) *Service {
	logger, ok := input.Logger.(*log.Logger)
	if !ok {
		logger = log.NewWithOptions(os.Stdout, log.Options{
			ReportCaller:    true,
			ReportTimestamp: true,
			Level:           log.DebugLevel,
		})
	}

	var natsClient *nats.Conn
	if input.NatsClient != nil {
		if nc, ok := input.NatsClient.(*nats.Conn); ok {
			natsClient = nc
		}
	}

	var queries pgdb.Querier
	if input.Queries != nil {
		if q, ok := input.Queries.(pgdb.Querier); ok {
			queries = q
		}
	}

	return &Service{
		Logger:           logger,
		Token:            input.Token,
		Client:           &http.Client{Timeout: time.Second * 45}, // Increased to 45 seconds to allow for 30s Telegram timeout + network overhead
		LastMessages:     []Message{},
		NatsClient:       natsClient,
		queries:          queries,
		messageCallbacks: make(map[string][]func(Message, string)),
	}
}

// Start begins polling for Telegram updates.
func (s *Service) Start(ctx context.Context) error {
	if s.Token == "" {
		return fmt.Errorf("telegram token not set")
	}

	lastUpdateID := 0
	s.Logger.Info("Starting telegram service")

	for {
		select {
		case <-ctx.Done():
			return nil
		default:
			url := fmt.Sprintf(
				"https://api.telegram.org/bot%s/getUpdates?offset=%d&timeout=30",
				s.Token,
				lastUpdateID+1,
			)

			req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
			if err != nil {
				s.Logger.Error("Failed to create request", "error", err)
				time.Sleep(time.Second * 5)
				continue
			}

			resp, err := s.Client.Do(req)
			if err != nil {
				s.Logger.Error("Failed to send request", "error", err)
				time.Sleep(time.Second * 5)
				continue
			}

			body, err := io.ReadAll(resp.Body)
			if err != nil {
				s.Logger.Error("failed to read response body", "error", err)
				if err := resp.Body.Close(); err != nil {
					s.Logger.Error("failed to close response body", "error", err)
				}
				time.Sleep(time.Second * 5)
				continue
			}

			err = resp.Body.Close()
			if err != nil {
				s.Logger.Error("failed to close response body", "error", err)
				time.Sleep(time.Second * 5)
				continue
			}

			var result struct {
				OK          bool     `json:"ok"`
				Result      []Update `json:"result"`
				Description string   `json:"description"`
				ErrorCode   int      `json:"error_code"`
			}

			s.Logger.Info("Received updates", "body", string(body))
			if err := json.Unmarshal(body, &result); err != nil {
				s.Logger.Error("Failed to decode response", "error", err)
				time.Sleep(time.Second * 5)
				continue
			}

			if !result.OK {
				s.Logger.Error("Telegram API returned error",
					"error_code", result.ErrorCode,
					"description", result.Description,
					"body", string(body),
				)
				time.Sleep(time.Second * 5)
				continue
			}

			for _, update := range result.Result {
				lastUpdateID = update.UpdateID

				// Look up chatUUID for logging
				chatID := update.Message.Chat.ID
				chatUUID, hasMapping := s.GetChatUUID(chatID)

				s.Logger.Info("Received message",
					"message_id", update.Message.MessageID,
					"from", update.Message.From.Username,
					"chat_id", chatID,
					"chat_uuid", chatUUID,
					"has_mapping", hasMapping,
					"text", update.Message.Text,
				)

				if update.Message.Text != "" {
					var chatUUID string
					chatID := update.Message.Chat.ID

					// Check for /start command with UUID
					if _, err := fmt.Sscanf(update.Message.Text, "/start %s", &chatUUID); err == nil {
						s.Logger.Info("Creating chat", "chat_id", chatID, "uuid", chatUUID)
						_, err := s.CreateChat(ctx, chatID, chatUUID)
						if err != nil {
							s.Logger.Error("Failed to create chat", "error", err)
							continue
						}
						err = s.SendMessage(ctx, chatID, "Send any message to start the conversation")
						if err != nil {
							s.Logger.Error("Failed to send message", "error", err)
							continue
						}
					}

					// Publish to NATS or notify callbacks if we have a chat mapping
					if chatUUID, exists := s.GetChatUUID(chatID); exists {
						if s.NatsClient != nil {
							// Publish to NATS if available
							subject := fmt.Sprintf("telegram.chat.%s", chatUUID)
							messageBytes, err := json.Marshal(update.Message)
							if err != nil {
								s.Logger.Error("Failed to marshal message", "error", err)
								continue
							}

							err = s.NatsClient.Publish(subject, messageBytes)
							if err != nil {
								s.Logger.Error("Failed to publish message to NATS", "error", err)
								continue
							}
							s.Logger.Info("Published message to NATS", "subject", subject, "chatUUID", chatUUID)
						} else {
							// Fallback: notify registered callbacks directly
							s.Logger.Info("NATS not available, using direct callbacks", "chatUUID", chatUUID)
							s.notifyCallbacks(chatUUID, update.Message)
						}
					} else {
						s.Logger.Debug("No chat mapping found for chatID, skipping message notification", "chat_id", chatID)
					}
				}
			}

			if len(result.Result) == 0 {
				time.Sleep(time.Second * 5)
			}
		}
	}
}

// CreateChat creates a mapping between chat ID and UUID.
func (s *Service) CreateChat(ctx context.Context, chatID int, chatUUID string) (int, error) {
	if s.queries == nil {
		return 0, fmt.Errorf("database queries not available")
	}

	params := pgdb.CreateTelegramChatParams{
		ChatID:   int64(chatID),
		ChatUuid: chatUUID,
	}

	_, err := s.queries.CreateTelegramChat(ctx, params)
	if err != nil {
		return 0, fmt.Errorf("failed to create chat mapping: %w", err)
	}

	s.Logger.Info("Chat mapping created", "chat_id", chatID, "uuid", chatUUID)
	return chatID, nil
}

// GetChatUUID returns the UUID for a given chat ID.
func (s *Service) GetChatUUID(chatID int) (string, bool) {
	if s.queries == nil {
		s.Logger.Error("Database queries not available")
		return "", false
	}

	ctx := context.Background()
	chat, err := s.queries.GetTelegramChatByChatID(ctx, int64(chatID))
	if err != nil {
		if err == sql.ErrNoRows {
			return "", false
		}
		s.Logger.Error("Failed to get chat UUID", "error", err, "chat_id", chatID)
		return "", false
	}

	return chat.ChatUuid, true
}

// GetChatIDByUUID returns the chat ID for a given UUID.
func (s *Service) GetChatIDByUUID(chatUUID string) (int, bool) {
	if s.queries == nil {
		s.Logger.Error("Database queries not available")
		return 0, false
	}

	ctx := context.Background()
	chat, err := s.queries.GetTelegramChatByChatUUID(ctx, chatUUID)
	if err != nil {
		if err == sql.ErrNoRows {
			return 0, false
		}
		s.Logger.Error("Failed to get chat ID", "error", err, "chat_uuid", chatUUID)
		return 0, false
	}

	return int(chat.ChatID), true
}

// SendMessage sends a message to a Telegram chat.
func (s *Service) SendMessage(ctx context.Context, chatID int, message string) error {
	url := fmt.Sprintf("%s/bot%s/sendMessage", TelegramAPIBase, s.Token)
	body := map[string]any{
		"chat_id":    chatID,
		"text":       message,
		"parse_mode": "HTML",
	}
	jsonBody, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal body: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewBuffer(jsonBody))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	s.Logger.Info("Sending message to Telegram", "url", url, "body", body)

	resp, err := s.Client.Do(req)
	if err != nil {
		return fmt.Errorf("send request: %w", err)
	}
	defer func() {
		err := resp.Body.Close()
		if err != nil {
			s.Logger.Warn("Failed to close response body", "error", err)
		}
	}()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("telegram API non-OK status: %d", resp.StatusCode)
	}

	var result struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}

	if !result.OK {
		return fmt.Errorf("telegram API error: %s", result.Description)
	}

	return nil
}

// Subscribe is a placeholder for internal GraphQL subscription handling.
func (s *Service) Subscribe(ctx context.Context, chatUUID string) error {
	if s.Logger == nil {
		return fmt.Errorf("logger is nil")
	}

	// TODO: This should be handled internally since this service IS the GraphQL server
	// For now, just log that subscription was requested
	s.Logger.Info("Telegram subscription requested for chat UUID", "chat_uuid", chatUUID)

	// This would be handled by the internal GraphQL subscription system
	// For now, return nil to indicate success
	return nil
}

// PostMessage is a placeholder for internal GraphQL mutation handling.
func (s *Service) PostMessage(ctx context.Context, chatUUID string, message string) (interface{}, error) {
	// TODO: This should be handled internally since this service IS the GraphQL server
	// For now, just log that a message post was requested
	s.Logger.Info("Telegram post message requested", "chat_uuid", chatUUID, "message", message)

	// This would be handled by the internal GraphQL mutation system
	// For now, return a success response
	return map[string]interface{}{
		"success": true,
		"message": "Message queued for posting",
	}, nil
}

// RegisterMessageCallback registers a callback function for messages in a specific chat UUID.
func (s *Service) RegisterMessageCallback(chatUUID string, callback func(Message, string)) string {
	s.callbacksMu.Lock()
	defer s.callbacksMu.Unlock()

	// Generate a unique callback ID
	callbackID := fmt.Sprintf("callback_%d", time.Now().UnixNano())

	if s.messageCallbacks[chatUUID] == nil {
		s.messageCallbacks[chatUUID] = make([]func(Message, string), 0)
	}

	// Wrap the callback to include the callback ID
	wrappedCallback := func(msg Message, uuid string) {
		callback(msg, uuid)
	}

	s.messageCallbacks[chatUUID] = append(s.messageCallbacks[chatUUID], wrappedCallback)
	s.Logger.Info("Registered message callback", "chatUUID", chatUUID, "callbackID", callbackID)

	return callbackID
}

// UnregisterMessageCallback removes a callback for a specific chat UUID.
func (s *Service) UnregisterMessageCallback(chatUUID string, callbackID string) {
	s.callbacksMu.Lock()
	defer s.callbacksMu.Unlock()

	// For simplicity, we'll clear all callbacks for the chatUUID
	// In a production system, you'd want to track callback IDs more precisely
	delete(s.messageCallbacks, chatUUID)
	s.Logger.Info("Unregistered message callback", "chatUUID", chatUUID, "callbackID", callbackID)
}

// notifyCallbacks calls all registered callbacks for a chat UUID.
func (s *Service) notifyCallbacks(chatUUID string, message Message) {
	s.callbacksMu.RLock()
	callbacks := s.messageCallbacks[chatUUID]
	s.callbacksMu.RUnlock()

	if len(callbacks) > 0 {
		s.Logger.Info("Notifying message callbacks", "chatUUID", chatUUID, "callbacks", len(callbacks))
		for _, callback := range callbacks {
			go func(cb func(Message, string)) {
				defer func() {
					if r := recover(); r != nil {
						s.Logger.Error("Callback panic recovered", "error", r)
					}
				}()
				cb(message, chatUUID)
			}(callback)
		}
	}
}
