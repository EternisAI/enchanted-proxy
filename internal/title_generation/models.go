package title_generation

// GenerateRequest contains the common parameters for title generation
type GenerateRequest struct {
	Model       string
	BaseURL     string
	APIKey      string
	UserContent string // The content to generate a title from
}

// RegenerationContext contains conversation context for improved title generation
type RegenerationContext struct {
	FirstUserMessage  string
	FirstAIResponse   string
	SecondUserMessage string
}

// StorageRequest contains all info needed to encrypt and store a generated title
type StorageRequest struct {
	UserID            string
	ChatID            string
	Title             string
	Platform          string
	EncryptionEnabled *bool
}
