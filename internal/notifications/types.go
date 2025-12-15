package notifications

// NotificationType represents the type of notification being sent.
type NotificationType string

const (
	TypeDeepResearch NotificationType = "deep_research"
	TypeGPT5Pro      NotificationType = "gpt5_pro"
)

// CompletionNotification represents a notification payload for a completed task.
type CompletionNotification struct {
	Title string
	Body  string
	Data  map[string]string
}

// TokenInfo represents a push notification token stored in Firestore.
type TokenInfo struct {
	Token         string `firestore:"token"`
	DeviceID      string `firestore:"deviceId"`
	LastUpdatedAt string `firestore:"lastUpdatedAt"`
}

// SendResult represents the result of sending a notification to a device.
type SendResult struct {
	Token    string
	Success  bool
	Response string
	Error    string
}
