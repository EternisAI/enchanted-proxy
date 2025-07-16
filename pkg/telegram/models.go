package telegram

const (
	// TelegramEnabled is the flag to enable telegram.
	TelegramEnabled = "telegram_enabled"
	// TelegramChatUUIDKey allows to identifies the chat with a specific user, after the first message.
	TelegramChatUUIDKey = "telegram_chat_uuid"
	// TelegramLastUpdateIDKey is used to track the last update ID for Telegram messages.
	TelegramLastUpdateIDKey = "telegram_last_update_id"
	// TelegramBotName is the telegram bot name to be used for sending messages.
	TelegramBotName = "MyTwinSlimBot"
	// TelegramAPIBase is the base url for the telegram api.
	TelegramAPIBase = "https://api.telegram.org"
)

// Update represents a Telegram update containing a message.
type Update struct {
	UpdateID int `json:"update_id"`
	Message  struct {
		MessageID int    `json:"message_id"`
		From      User   `json:"from"`
		Chat      Chat   `json:"chat"`
		Date      int    `json:"date"`
		Text      string `json:"text"`
	} `json:"message"`
}

// User represents a Telegram user.
type User struct {
	ID        int    `json:"id"`
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name"`
	Username  string `json:"username"`
}

// Chat represents a Telegram chat.
type Chat struct {
	ID        int    `json:"id"`
	Type      string `json:"type"`
	Title     string `json:"title"`
	Username  string `json:"username"`
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name"`
}

// Message represents a Telegram message.
type Message struct {
	MessageID int    `json:"message_id"`
	From      User   `json:"from"`
	Chat      Chat   `json:"chat"`
	Date      int    `json:"date"`
	Text      string `json:"text"`
}

// GetUpdatesResponse represents the response from Telegram's getUpdates API.
type GetUpdatesResponse struct {
	OK     bool      `json:"ok"`
	Result []Message `json:"result"`
}

// TelegramServiceInput contains the dependencies needed to create a TelegramService.
type TelegramServiceInput struct {
	Logger     interface{} // Using interface{} to avoid import conflicts
	Token      string
	Store      interface{} // Will be the database store
	Queries    interface{} // Database queries interface
	NatsClient interface{} // NATS client for pub/sub
}

// WebSocketMessage represents the structure of messages received from GraphQL subscriptions.
type WebSocketMessage struct {
	ID        string
	Text      *string
	Role      string
	CreatedAt string
}

// GraphQLResponse represents a GraphQL response.
type GraphQLResponse struct {
	Data   interface{} `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

// SendMessageRequest represents a request to send a Telegram message.
type SendMessageRequest struct {
	ChatID   int    `json:"chat_id" binding:"required"`
	Text     string `json:"text" binding:"required"`
	ChatUUID string `json:"chat_uuid"`
}

// CreateChatRequest represents a request to create a new chat.
type CreateChatRequest struct {
	ChatID   int    `json:"chat_id" binding:"required"`
	ChatUUID string `json:"chat_uuid" binding:"required"`
}

// SubscribeRequest represents a request to subscribe to chat updates.
type SubscribeRequest struct {
	ChatUUID string `json:"chat_uuid" binding:"required"`
}

// ChatURLResponse represents the response containing a chat URL.
type ChatURLResponse struct {
	URL string `json:"url"`
}
