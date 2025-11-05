package title_generation

// TitleGenerationRequest represents a request to generate a chat title
type TitleGenerationRequest struct {
	UserID            string
	ChatID            string
	FirstMessage      string
	Model             string
	BaseURL           string
	Platform          string
	EncryptionEnabled *bool // nil = not specified (backward compat), true = enforce encryption, false = store plaintext
}

// TitleGenerationResponse represents the generated title
type TitleGenerationResponse struct {
	Title     string
	Encrypted bool
	Error     error
}
