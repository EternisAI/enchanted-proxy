package probe

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// slackNotifier sends probe state-change notifications to a Slack webhook.
type slackNotifier struct {
	webhookURL string
	httpClient *http.Client
}

func newSlackNotifier(webhookURL string) *slackNotifier {
	return &slackNotifier{
		webhookURL: webhookURL,
		httpClient: &http.Client{Timeout: 10 * time.Second},
	}
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

// sendProbeNotification sends a formatted Slack message for a probe state change.
func (n *slackNotifier) sendProbeNotification(ctx context.Context, provider, model string, result probeResult) error {
	var emoji, status, fallbackText string
	var detailLines []string

	if result.success {
		emoji = "\u2705" // green check
		status = "Probe Succeeded"
		fallbackText = fmt.Sprintf("Probe succeeded: %s / %s", provider, model)

		detailLines = append(detailLines,
			fmt.Sprintf("*Status:* `%d`", result.statusCode),
			fmt.Sprintf("*Duration:* `%s`", result.duration.Round(time.Millisecond)),
		)
		if result.usage != nil {
			detailLines = append(detailLines,
				fmt.Sprintf("*Tokens:* prompt=%d, completion=%d", result.usage.PromptTokens, result.usage.CompletionTokens),
			)
		}
	} else {
		emoji = "\u274c" // red X
		status = "Probe Failed"
		fallbackText = fmt.Sprintf("Probe failed: %s / %s", provider, model)

		if result.err != nil {
			detailLines = append(detailLines,
				fmt.Sprintf("*Error:* `%s`", result.err.Error()),
			)
			if result.duration > 0 {
				detailLines = append(detailLines,
					fmt.Sprintf("*Duration:* `%s`", result.duration.Round(time.Millisecond)),
				)
			}
		} else {
			detailLines = append(detailLines,
				fmt.Sprintf("*Status:* `%d`", result.statusCode),
				fmt.Sprintf("*Duration:* `%s`", result.duration.Round(time.Millisecond)),
			)
			if result.contentMismatch {
				detailLines = append(detailLines,
					fmt.Sprintf("*Content mismatch:* expected `%s`, got `%s`", result.expected, result.got),
				)
			} else if result.body != "" {
				sanitized := strings.ReplaceAll(result.body, "`", "'")
				sanitized = strings.ReplaceAll(sanitized, "\n", " ")
				detailLines = append(detailLines,
					fmt.Sprintf("*Response:* `%s`", sanitized),
				)
			}
		}
	}

	details := strings.Join(detailLines, "\n")

	blocks := []slackBlock{
		{
			Type: "header",
			Text: &slackText{Type: "plain_text", Text: fmt.Sprintf("%s %s", emoji, status)},
		},
		{
			Type: "section",
			Text: &slackText{Type: "mrkdwn", Text: fmt.Sprintf("*Provider:* `%s`\n*Model:* `%s`", provider, model)},
		},
		{
			Type: "section",
			Text: &slackText{Type: "mrkdwn", Text: details},
		},
	}

	msg := slackMessage{
		Text:   fallbackText,
		Blocks: blocks,
	}

	body, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("marshal slack message: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", n.webhookURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create slack request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := n.httpClient.Do(httpReq)
	if err != nil {
		return fmt.Errorf("send slack notification: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("slack webhook returned status %d", resp.StatusCode)
	}

	return nil
}
