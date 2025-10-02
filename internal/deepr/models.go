package deepr

// Message represents a WebSocket message for deep research
type Message struct {
	Type    string `json:"type"`
	Content string `json:"content"`
	Data    string `json:"data,omitempty"`
}

// Request represents a request to the deep research service
type Request struct {
	Query string `json:"query"`
	Type  string `json:"type"`
}

// Response represents a response from the deep research service
type Response struct {
	Type    string `json:"type"`
	Content string `json:"content"`
	Status  string `json:"status,omitempty"`
}
