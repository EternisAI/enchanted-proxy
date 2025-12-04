package common

import "encoding/json"

// ExtractModelFromRequestBody extracts the model field from request body bytes.
// This implementation uses json.Unmarshal for accuracy and consistency.
func ExtractModelFromRequestBody(path string, body []byte) string {
	if path != "/chat/completions" {
		return ""
	}

	if len(body) == 0 {
		return ""
	}

	var requestData struct {
		Model string `json:"model"`
	}

	if err := json.Unmarshal(body, &requestData); err != nil {
		return ""
	}

	return requestData.Model
}
