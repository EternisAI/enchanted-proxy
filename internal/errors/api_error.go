package errors

// APIError represents a simple standardized error response.
// Used for 400, 401, 404, 409, 500 errors that don't need specialized shapes.
type APIError struct {
	Error   string                 `json:"error"`
	Details map[string]interface{} `json:"details,omitempty"`
}

// NewAPIError creates a new APIError with the given message and optional details.
func NewAPIError(message string, details map[string]interface{}) *APIError {
	return &APIError{
		Error:   message,
		Details: details,
	}
}
