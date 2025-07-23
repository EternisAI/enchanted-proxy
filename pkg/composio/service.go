package composio

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/eternisai/enchanted-proxy/pkg/config"
	"github.com/eternisai/enchanted-proxy/pkg/logger"
)

const (
	ComposioBaseURL = "https://backend.composio.dev/api/v3"
)

type Service struct {
	logger     *logger.Logger
	httpClient *http.Client
	apiKey     string
}

// NewService creates a new instance of Composio Service.
func NewService(logger *logger.Logger) *Service {
	return &Service{
		logger: logger,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
		apiKey: config.AppConfig.ComposioAPIKey,
	}
}

func (s *Service) getComposioConfig(toolkitSlug string) (string, error) {
	switch toolkitSlug {
	case "twitter":
		return config.AppConfig.ComposioTwitterConfig, nil
	default:
		return "", fmt.Errorf("unsupported toolkit slug: %s", toolkitSlug)
	}
}

// CreateConnectedAccount creates a new connected account and returns the redirect URL.
func (s *Service) CreateConnectedAccount(userID, toolkitSlug, callbackURL string) (*CreateConnectedAccountResponse, error) {
	s.logger.Info("creating composio connected account",
		slog.String("user_id", userID),
		slog.String("toolkit_slug", toolkitSlug))

	if s.apiKey == "" {
		return nil, fmt.Errorf("composio API key is not configured")
	}

	// Based on the API documentation, we need to use the v2 endpoint for initiate connection
	// as v1 is deprecated. However, since we're implementing v3 service, we'll use the
	// connected accounts endpoint pattern but adapt it for the current API structure
	url := fmt.Sprintf("%s/connected_accounts", ComposioBaseURL)
	composioConfig, err := s.getComposioConfig(toolkitSlug)
	if err != nil {
		return nil, fmt.Errorf("failed to get composio config: %w", err)
	}

	// Prepare request payload
	payload := map[string]interface{}{
		"auth_config": map[string]interface{}{
			"id": composioConfig,
		},
		"connection": map[string]interface{}{
			"user_id":      userID,
			"callback_url": callbackURL,
		},
	}
	jsonPayload, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request payload: %w", err)
	}

	// Create HTTP request
	ctx := context.Background()
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewBuffer(jsonPayload))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	// Set headers
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", s.apiKey)

	// Execute request
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to execute request: %w", err)
	}
	defer func() {
		if closeErr := resp.Body.Close(); closeErr != nil {
			s.logger.Warn("failed to close response body", slog.String("error", closeErr.Error()))
		}
	}()

	// Read response body
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response body: %w", err)
	}

	// Check for HTTP errors
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		var composioErr ComposioError
		if jsonErr := json.Unmarshal(body, &composioErr); jsonErr == nil {
			return nil, composioErr
		}
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
	}

	// Parse successful response
	var response ConnectedAccountResponse
	if err := json.Unmarshal(body, &response); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}

	return &CreateConnectedAccountResponse{
		ID:  response.ID,
		URL: response.RedirectURL,
	}, nil
}

func (s *Service) GetConnectedAccount(accountID string) (*ConnectedAccountDetailResponse, error) {
	if s.apiKey == "" {
		return nil, fmt.Errorf("composio API key is not configured")
	}

	// Based on the API documentation, we need to use the v2 endpoint for initiate connection
	// as v1 is deprecated. However, since we're implementing v3 service, we'll use the
	// connected accounts endpoint pattern but adapt it for the current API structure
	url := fmt.Sprintf("%s/connected_accounts/%s", ComposioBaseURL, accountID)

	// Create HTTP request
	ctx := context.Background()
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	// Set headers
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", s.apiKey)
	// Execute request
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to execute request: %w", err)
	}
	defer func() {
		if closeErr := resp.Body.Close(); closeErr != nil {
			s.logger.Warn("failed to close response body", slog.String("error", closeErr.Error()))
		}
	}()

	// Read response body
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response body: %w", err)
	}

	// Check for HTTP errors
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		var composioErr ComposioError
		if jsonErr := json.Unmarshal(body, &composioErr); jsonErr == nil {
			return nil, composioErr
		}
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
	}

	// Parse successful response
	var response ConnectedAccountDetailResponse
	if err := json.Unmarshal(body, &response); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}

	return &response, nil
}

func (s *Service) RefreshToken(accountID string) (*ComposioRefreshTokenResponse, error) {
	if s.apiKey == "" {
		return nil, fmt.Errorf("composio API key is not configured")
	}

	// Based on the API documentation, we need to use the v2 endpoint for initiate connection
	// as v1 is deprecated. However, since we're implementing v3 service, we'll use the
	// connected accounts endpoint pattern but adapt it for the current API structure
	url := fmt.Sprintf("%s/connected_accounts/%s/refresh", ComposioBaseURL, accountID)

	// Create HTTP request
	ctx := context.Background()
	req, err := http.NewRequestWithContext(ctx, "POST", url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	// Set headers
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", s.apiKey)

	// Execute request
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to execute request: %w", err)
	}
	defer func() {
		if closeErr := resp.Body.Close(); closeErr != nil {
			s.logger.Warn("failed to close response body", slog.String("error", closeErr.Error()))
		}
	}()

	// Read response body
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response body: %w", err)
	}

	// Check for HTTP errors
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		var composioErr ComposioError
		if jsonErr := json.Unmarshal(body, &composioErr); jsonErr == nil {
			return nil, composioErr
		}
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
	}

	// Parse successful response
	var response ComposioRefreshTokenResponse
	if err := json.Unmarshal(body, &response); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}

	return &response, nil
}
