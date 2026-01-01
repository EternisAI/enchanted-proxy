package problem_reports

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

type LinearClient struct {
	apiKey     string
	teamID     string
	projectID  string
	labelID    string
	httpClient *http.Client
}

func NewLinearClient(apiKey, teamID, projectID, labelID string) *LinearClient {
	return &LinearClient{
		apiKey:     apiKey,
		teamID:     teamID,
		projectID:  projectID,
		labelID:    labelID,
		httpClient: &http.Client{Timeout: 30 * time.Second},
	}
}

type graphqlRequest struct {
	Query     string         `json:"query"`
	Variables map[string]any `json:"variables,omitempty"`
}

type createIssueResponse struct {
	Data struct {
		IssueCreate struct {
			Success bool `json:"success"`
			Issue   struct {
				ID         string `json:"id"`
				Identifier string `json:"identifier"`
			} `json:"issue"`
		} `json:"issueCreate"`
	} `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

func (c *LinearClient) CreateIssue(ctx context.Context, title, description string) (string, error) {
	query := `
		mutation IssueCreate($input: IssueCreateInput!) {
			issueCreate(input: $input) {
				success
				issue {
					id
					identifier
				}
			}
		}
	`

	input := map[string]any{
		"title":       title,
		"description": description,
		"teamId":      c.teamID,
		"projectId":   c.projectID,
	}
	if c.labelID != "" {
		input["labelIds"] = []string{c.labelID}
	}

	variables := map[string]any{
		"input": input,
	}

	reqBody := graphqlRequest{
		Query:     query,
		Variables: variables,
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("failed to marshal Linear request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", "https://api.linear.app/graphql", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("failed to create Linear request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", c.apiKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("failed to call Linear API: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("Linear API returned status %d: %s", resp.StatusCode, string(respBody))
	}

	var issueResp createIssueResponse
	if err := json.NewDecoder(resp.Body).Decode(&issueResp); err != nil {
		return "", fmt.Errorf("failed to decode Linear response: %w", err)
	}

	if len(issueResp.Errors) > 0 {
		return "", fmt.Errorf("Linear API error: %s", issueResp.Errors[0].Message)
	}

	if !issueResp.Data.IssueCreate.Success {
		return "", fmt.Errorf("Linear issue creation failed")
	}

	return issueResp.Data.IssueCreate.Issue.Identifier, nil
}
