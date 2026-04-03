package problem_reports

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

type SlackNotifier struct {
	webhookURL string
	httpClient *http.Client
}

func NewSlackNotifier(webhookURL string) *SlackNotifier {
	return &SlackNotifier{
		webhookURL: webhookURL,
		httpClient: &http.Client{Timeout: 10 * time.Second},
	}
}

func (n *SlackNotifier) Enabled() bool {
	return n.webhookURL != ""
}

type slackMessage struct {
	Text   string       `json:"text"`
	Blocks []slackBlock `json:"blocks,omitempty"`
}

type slackBlock struct {
	Type string     `json:"type"`
	Text *slackText `json:"text,omitempty"`
}

type slackText struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func (n *SlackNotifier) SendReport(ctx context.Context, req *CreateProblemReportRequest, reportID, userID string, linearTicketID string) error {
	description := truncateString(req.ProblemDescription, 500)

	var deviceLine string
	if req.DeviceInfo != nil {
		deviceLine = fmt.Sprintf("*Device:* %s (%s %s) · App %s (%s)",
			req.DeviceInfo.DeviceModel,
			req.DeviceInfo.SystemName,
			req.DeviceInfo.SystemVersion,
			req.DeviceInfo.AppVersion,
			req.DeviceInfo.BuildNumber)
	} else {
		deviceLine = "*Device:* not provided"
	}

	tier := ptrToString(req.SubscriptionTier, "unknown")
	email := ptrToString(req.ContactEmail, "not provided")

	var ticketLine string
	if linearTicketID != "" {
		ticketLine = fmt.Sprintf("*Linear:* %s", linearTicketID)
	} else {
		ticketLine = "*Linear:* failed to create"
	}

	blocks := []slackBlock{
		{
			Type: "header",
			Text: &slackText{Type: "plain_text", Text: "🐛 Problem Report"},
		},
		{
			Type: "section",
			Text: &slackText{Type: "mrkdwn", Text: fmt.Sprintf("*Description:*\n%s", description)},
		},
		{
			Type: "section",
			Text: &slackText{Type: "mrkdwn", Text: fmt.Sprintf("%s\n*Tier:* %s · *Email:* %s\n%s\n*Report ID:* `%s`",
				deviceLine, tier, email, ticketLine, reportID)},
		},
	}

	msg := slackMessage{
		Text:   fmt.Sprintf("New problem report: %s", truncateString(req.ProblemDescription, 80)),
		Blocks: blocks,
	}

	body, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("failed to marshal Slack message: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", n.webhookURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("failed to create Slack request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := n.httpClient.Do(httpReq)
	if err != nil {
		return fmt.Errorf("failed to send Slack notification: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("Slack webhook returned status %d", resp.StatusCode)
	}

	return nil
}
