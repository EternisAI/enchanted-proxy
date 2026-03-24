package anonymizer

// Replacement represents a single PII entity replacement.
type Replacement struct {
	Original    string `json:"original"`
	Replacement string `json:"replacement"`
}
