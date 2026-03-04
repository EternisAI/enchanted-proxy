package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/eternisai/enchanted-proxy/internal/auth"
	"github.com/eternisai/enchanted-proxy/internal/logger"
	"github.com/eternisai/enchanted-proxy/internal/messaging"
	"github.com/eternisai/enchanted-proxy/internal/request_tracking"
	"github.com/eternisai/enchanted-proxy/internal/routing"
	"github.com/eternisai/enchanted-proxy/internal/streaming"
	"github.com/eternisai/enchanted-proxy/internal/tools"
	"github.com/gin-gonic/gin"
)

const (
	// maxSimpleChunkSize is the max size of a single SSE line in the simple streaming path.
	maxSimpleChunkSize = 1024 * 1024 // 1MB

	// maxSimpleContinuations is the max number of tool call continuations.
	maxSimpleContinuations = 5
)

// handleStreamingSimple is a plain pass-through streaming handler.
//
// Unlike handleStreamingDirect (which uses StreamManager/Session/Subscriber for
// multicast broadcast), this function simply:
//  1. Makes an upstream HTTP request with a cancellable background context
//  2. Reads SSE lines from the upstream response
//  3. Writes each line directly to the client's HTTP response
//  4. Accumulates content for Firestore save
//  5. Handles tool call continuation (agentic loop)
//  6. Registers a cancel function so the stop endpoint can cancel the upstream
//
// The upstream request uses context.Background() so it can survive client disconnect
// long enough to save the accumulated content to Firestore.
func handleStreamingSimple(
	c *gin.Context,
	target *url.URL,
	apiKey string,
	requestBody []byte,
	log *logger.Logger,
	start time.Time,
	model string,
	trackingService *request_tracking.Service,
	messageService *messaging.Service,
	cancelRegistry *CancelRegistry,
	toolExecutor *streaming.ToolExecutor,
	_ *tools.Registry, // unused but kept for signature consistency; tools are injected into requestBody by caller
	provider *routing.ProviderConfig,
) {
	// --- Extract IDs ---
	chatID := c.GetHeader("X-Chat-ID")
	messageID := c.GetHeader("X-Message-ID")
	if chatID == "" {
		if v, ok := c.Get("bodyChatId"); ok {
			if s, ok := v.(string); ok {
				chatID = s
			}
		}
	}
	if messageID == "" {
		if v, ok := c.Get("bodyMessageId"); ok {
			if s, ok := v.(string); ok {
				messageID = s
			}
		}
	}
	if chatID == "" {
		chatID = fmt.Sprintf("chat-%d", time.Now().UnixNano())
	}
	if messageID == "" {
		messageID = fmt.Sprintf("msg-%d", time.Now().UnixNano())
	}

	userID, _ := auth.GetUserID(c)
	var encryptionEnabled *bool
	if val, exists := c.Get("encryptionEnabled"); exists {
		if boolPtr, ok := val.(*bool); ok {
			encryptionEnabled = boolPtr
		}
	}

	// --- Cancellable context for upstream (independent of client) ---
	upstreamCtx, upstreamCancel := context.WithCancel(context.Background())
	defer upstreamCancel()

	// Register cancel so the stop endpoint can find it.
	if cancelRegistry != nil {
		cancelRegistry.Register(chatID, messageID, upstreamCancel)
		defer cancelRegistry.Remove(chatID, messageID)
	}

	// Track whether the user stopped the stream
	var stopped bool

	// --- Build upstream URL ---
	requestPath := c.Request.URL.Path
	upstreamURL := target.String() + requestPath

	// --- Independent HTTP client (HTTP/1.1, no shared transport) ---
	client := &http.Client{
		Transport: &http.Transport{
			MaxIdleConns:          100,
			MaxIdleConnsPerHost:   10,
			IdleConnTimeout:       90 * time.Second,
			DisableKeepAlives:     false,
			DisableCompression:    true,
			ForceAttemptHTTP2:     false,
			DialContext:           (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			TLSHandshakeTimeout:  30 * time.Second,
			ResponseHeaderTimeout: 120 * time.Second,
		},
		Timeout: 0,
	}

	// --- Make initial upstream request ---
	resp, err := doUpstreamRequest(upstreamCtx, client, upstreamURL, apiKey, requestBody)
	if err != nil {
		log.Error("simple streaming: upstream request failed",
			slog.String("error", err.Error()),
			slog.String("chat_id", chatID))
		c.JSON(http.StatusBadGateway, gin.H{"error": "Failed to connect to upstream provider"})
		return
	}

	// Check for upstream errors
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		log.Error("simple streaming: upstream error",
			slog.Int("status", resp.StatusCode),
			slog.String("chat_id", chatID))
		c.Data(resp.StatusCode, "application/json", body)
		return
	}

	upstreamLatency := time.Since(start)
	log.Info("simple streaming: upstream responded",
		slog.String("chat_id", chatID),
		slog.Int("status", resp.StatusCode),
		slog.Duration("latency", upstreamLatency))

	// --- Set SSE headers and write 200 OK ---
	c.Writer.Header().Set("Content-Type", "text/event-stream")
	c.Writer.Header().Set("Cache-Control", "no-cache")
	c.Writer.Header().Set("Connection", "keep-alive")
	c.Writer.Header().Set("X-Accel-Buffering", "no")
	c.Writer.WriteHeader(http.StatusOK)

	flusher, hasFlusher := c.Writer.(http.Flusher)

	// --- Log request body details for debugging ---
	var reqSummary struct {
		Model            string      `json:"model"`
		MaxTokens        interface{} `json:"max_tokens"`
		MaxCompTokens    interface{} `json:"max_completion_tokens"`
		Stream           interface{} `json:"stream"`
		MessageCount     int         `json:"-"`
	}
	if err := json.Unmarshal(requestBody, &reqSummary); err == nil {
		var reqMap map[string]interface{}
		json.Unmarshal(requestBody, &reqMap)
		if msgs, ok := reqMap["messages"].([]interface{}); ok {
			reqSummary.MessageCount = len(msgs)
		}
		log.Info("simple streaming: request details",
			slog.String("chat_id", chatID),
			slog.String("model", fmt.Sprintf("%v", reqSummary.Model)),
			slog.Any("max_tokens", reqSummary.MaxTokens),
			slog.Any("max_completion_tokens", reqSummary.MaxCompTokens),
			slog.Int("message_count", reqSummary.MessageCount),
			slog.String("upstream_url", upstreamURL))
	}

	// --- State for the streaming loop ---
	var content strings.Builder
	var tokenUsage *streaming.TokenUsage
	var finishReason string
	var chunkCount int
	clientGone := false
	continuations := 0

	// GLM content filter
	var glmFilter *streaming.GLMContentFilter
	if strings.Contains(strings.ToLower(model), "glm") {
		glmFilter = streaming.NewGLMContentFilter()
	}

	// Current upstream body (may change on tool continuation)
	currentBody := resp.Body

	// --- Main streaming loop (may loop on tool continuations) ---
streamLoop:
	for {
		scanner := bufio.NewScanner(currentBody)
		scanner.Buffer(make([]byte, 64*1024), maxSimpleChunkSize)

		var toolDetector *streaming.ToolCallDetector
		if toolExecutor != nil {
			toolDetector = streaming.NewToolCallDetector()
		}

		for scanner.Scan() {
			// Check upstream cancellation (user stop)
			select {
			case <-upstreamCtx.Done():
				stopped = true
				log.Info("simple streaming: stopped by user",
					slog.String("chat_id", chatID))
				break streamLoop
			default:
			}

			line := scanner.Text()
			if strings.TrimSpace(line) == "" {
				continue
			}

			// Apply GLM content filter
			if glmFilter != nil {
				if filtered, wasFiltered := glmFilter.FilterSSELine(line); wasFiltered {
					line = filtered
				}
			}

			// Normalize non-JSON error lines into valid OpenAI-format SSE chunks
			if normalized, changed := streaming.NormalizeSSEErrorLine(line); changed {
				line = normalized
				log.Warn("simple streaming: normalized upstream SSE error",
					slog.String("chat_id", chatID))
			}

			// Normalize reasoning_content → reasoning
			if normalized, changed := streaming.NormalizeReasoningField(line); changed {
				line = normalized
			}

			chunkCount++

			// Extract token usage
			if usage := extractSimpleTokenUsage(line); usage != nil {
				tokenUsage = usage
			}

			// Extract finish_reason
			if fr := extractFinishReason(line); fr != "" {
				finishReason = fr
				log.Info("simple streaming: finish_reason received",
					slog.String("chat_id", chatID),
					slog.String("finish_reason", fr),
					slog.Int("chunk_count", chunkCount),
					slog.Int("content_length", content.Len()))
			}

			// Accumulate content for Firestore save
			if c := extractContentFromSSELine(line); c != "" {
				content.WriteString(c)
			}

			// Tool call detection
			isToolChunk := false
			if toolDetector != nil {
				isToolChunk = toolDetector.ProcessChunk(line)
			}

			// Check for [DONE]
			isFinal := strings.Contains(line, "[DONE]")

			// Write to client (unless it's a tool call chunk being suppressed, or client is gone)
			if !isToolChunk && !clientGone {
				if _, err := c.Writer.WriteString(line + "\n"); err != nil {
					clientGone = true
					log.Info("simple streaming: client disconnected",
						slog.String("chat_id", chatID))
				} else if hasFlusher {
					flusher.Flush()
				}
			}

			// If tool calls complete, execute them and continue
			if toolDetector != nil && toolDetector.IsComplete() {
				currentBody.Close()

				if continuations >= maxSimpleContinuations {
					log.Warn("simple streaming: max continuations reached",
						slog.String("chat_id", chatID))
					sendSimpleError(c, &clientGone, flusher, hasFlusher,
						fmt.Sprintf("Maximum tool continuation depth (%d) reached", maxSimpleContinuations))
					sendSimpleDone(c, &clientGone, flusher, hasFlusher)
					break streamLoop
				}

				// Execute tools
				toolCalls := toolDetector.GetToolCalls()
				log.Info("simple streaming: executing tools",
					slog.String("chat_id", chatID),
					slog.Int("count", len(toolCalls)))

				// Send tool notifications to client
				onNotification := func(notif streaming.ToolNotification) {
					sendSimpleToolNotification(c, &clientGone, flusher, hasFlusher, notif)
				}

				toolCtx := upstreamCtx
				if userID != "" {
					toolCtx = contextWithUserAndChat(upstreamCtx, userID, chatID)
				}

				toolResults, err := toolExecutor.ExecuteToolCalls(toolCtx, chatID, messageID, toolCalls, onNotification)
				if err != nil {
					log.Error("simple streaming: tool execution failed",
						slog.String("error", err.Error()),
						slog.String("chat_id", chatID))
				}

				// Build continuation request
				var originalReq map[string]interface{}
				if err := json.Unmarshal(requestBody, &originalReq); err != nil {
					log.Error("simple streaming: failed to parse original request", slog.String("error", err.Error()))
					sendSimpleError(c, &clientGone, flusher, hasFlusher, "Failed to process tool results")
					sendSimpleDone(c, &clientGone, flusher, hasFlusher)
					break streamLoop
				}
				originalMessages, ok := originalReq["messages"].([]interface{})
				if !ok {
					sendSimpleError(c, &clientGone, flusher, hasFlusher, "Failed to process tool results")
					sendSimpleDone(c, &clientGone, flusher, hasFlusher)
					break streamLoop
				}

				// Build assistant message with tool calls
				toolCallsForMsg := make([]map[string]interface{}, len(toolCalls))
				for i, tc := range toolCalls {
					toolCallsForMsg[i] = map[string]interface{}{
						"id":   tc.ID,
						"type": tc.Type,
						"function": map[string]interface{}{
							"name":      tc.Function.Name,
							"arguments": tc.Function.Arguments,
						},
					}
				}
				assistantMsg := map[string]interface{}{
					"role":       "assistant",
					"content":    nil,
					"tool_calls": toolCallsForMsg,
				}

				contBody, err := toolExecutor.CreateContinuationRequest(
					upstreamCtx, target.String(), apiKey,
					originalReq, originalMessages, assistantMsg, toolResults,
				)
				if err != nil {
					log.Error("simple streaming: continuation request failed",
						slog.String("error", err.Error()),
						slog.String("chat_id", chatID))
					sendSimpleError(c, &clientGone, flusher, hasFlusher,
						fmt.Sprintf("Error processing tool results: %s", err.Error()))
					sendSimpleDone(c, &clientGone, flusher, hasFlusher)
					break streamLoop
				}

				continuations++
				currentBody = contBody
				log.Info("simple streaming: continuing with tool results",
					slog.String("chat_id", chatID),
					slog.Int("continuation", continuations))
				continue streamLoop // Start reading from new body
			}

			if isFinal {
				break streamLoop
			}
		}

		// Scanner finished (EOF or error) without [DONE] or tool call
		if err := scanner.Err(); err != nil {
			if !strings.Contains(err.Error(), "context canceled") {
				log.Error("simple streaming: scanner error",
					slog.String("error", err.Error()),
					slog.String("chat_id", chatID),
					slog.Int("chunk_count", chunkCount),
					slog.Int("content_length", content.Len()))
			}
		} else {
			log.Info("simple streaming: upstream EOF",
				slog.String("chat_id", chatID),
				slog.Int("chunk_count", chunkCount),
				slog.Int("content_length", content.Len()),
				slog.String("finish_reason", finishReason))
		}
		currentBody.Close()
		break streamLoop
	}

	// --- Post-stream: save to Firestore and log tokens ---
	log.Info("simple streaming: completed",
		slog.String("chat_id", chatID),
		slog.String("message_id", messageID),
		slog.Int("content_length", content.Len()),
		slog.Int("chunk_count", chunkCount),
		slog.String("finish_reason", finishReason),
		slog.Bool("stopped", stopped),
		slog.Bool("client_gone", clientGone),
		slog.Duration("total_duration", time.Since(start)))

	if tokenUsage != nil {
		log.Info("simple streaming: token usage",
			slog.String("chat_id", chatID),
			slog.Int("prompt_tokens", tokenUsage.PromptTokens),
			slog.Int("completion_tokens", tokenUsage.CompletionTokens),
			slog.Int("total_tokens", tokenUsage.TotalTokens))
	}

	// Save message to Firestore
	if messageService != nil && userID != "" && content.Len() > 0 {
		now := time.Now()
		generationState := "completed"
		if stopped {
			generationState = "completed" // Still save what we have
		}

		msg := messaging.MessageToStore{
			UserID:                userID,
			ChatID:                chatID,
			MessageID:             messageID,
			IsFromUser:            false,
			Content:               content.String(),
			IsError:               false,
			EncryptionEnabled:     encryptionEnabled,
			Stopped:               stopped,
			Model:                 model,
			GenerationState:       generationState,
			GenerationCompletedAt: &now,
		}
		if err := messageService.StoreMessageAsync(context.Background(), msg); err != nil {
			log.Error("simple streaming: failed to save message",
				slog.String("error", err.Error()),
				slog.String("chat_id", chatID))
		}
	}

	// Log token usage
	if tokenUsage != nil && trackingService != nil && provider != nil {
		tokenData := &request_tracking.TokenUsageWithMultiplier{
			PromptTokens:     tokenUsage.PromptTokens,
			CompletionTokens: tokenUsage.CompletionTokens,
			TotalTokens:      tokenUsage.TotalTokens,
			Multiplier:       provider.TokenMultiplier,
			PlanTokens:       int(float64(tokenUsage.TotalTokens) * provider.TokenMultiplier),
		}
		info := request_tracking.RequestInfo{
			UserID:   userID,
			Endpoint: requestPath,
			Model:    model,
			Provider: provider.Name,
		}
		trackingService.LogRequestWithPlanTokensAsync(context.Background(), info, tokenData) //nolint:errcheck
	} else if trackingService != nil {
		info := request_tracking.RequestInfo{
			UserID:   userID,
			Endpoint: requestPath,
			Model:    model,
			Provider: provider.Name,
		}
		trackingService.LogRequestAsync(context.Background(), info) //nolint:errcheck
	}
}

// doUpstreamRequest makes the HTTP request to the AI provider.
func doUpstreamRequest(ctx context.Context, client *http.Client, url, apiKey string, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("User-Agent", "Mozilla/5.0")
	req.Header.Set("Accept-Encoding", "identity")
	req.ContentLength = int64(len(body))
	return client.Do(req)
}

// extractFinishReason extracts the finish_reason from an SSE line.
// Returns empty string if not present or not parseable.
func extractFinishReason(line string) string {
	if !strings.HasPrefix(line, "data: ") {
		return ""
	}
	data := strings.TrimPrefix(line, "data: ")
	if data == "[DONE]" {
		return ""
	}
	var chunk struct {
		Choices []struct {
			FinishReason *string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal([]byte(data), &chunk); err != nil {
		return ""
	}
	if len(chunk.Choices) > 0 && chunk.Choices[0].FinishReason != nil {
		return *chunk.Choices[0].FinishReason
	}
	return ""
}

// extractSimpleTokenUsage extracts token usage from an SSE line.
func extractSimpleTokenUsage(line string) *streaming.TokenUsage {
	if !strings.HasPrefix(line, "data: ") {
		return nil
	}
	data := strings.TrimPrefix(line, "data: ")
	if data == "[DONE]" {
		return nil
	}
	var chunk struct {
		Usage *struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			TotalTokens      int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal([]byte(data), &chunk); err != nil || chunk.Usage == nil {
		return nil
	}
	return &streaming.TokenUsage{
		PromptTokens:     chunk.Usage.PromptTokens,
		CompletionTokens: chunk.Usage.CompletionTokens,
		TotalTokens:      chunk.Usage.TotalTokens,
	}
}

// sendSimpleToolNotification sends a tool notification SSE event to the client.
func sendSimpleToolNotification(c *gin.Context, clientGone *bool, flusher http.Flusher, hasFlusher bool, notif streaming.ToolNotification) {
	if *clientGone {
		return
	}
	notifJSON, err := json.Marshal(map[string]interface{}{
		"type":         "tool_notification",
		"event":        notif.Event,
		"tool_name":    notif.ToolName,
		"tool_call_id": notif.ToolCallID,
		"query":        notif.Query,
		"summary":      notif.Summary,
		"error":        notif.Error,
	})
	if err != nil {
		return
	}
	if _, err := c.Writer.WriteString("data: " + string(notifJSON) + "\n"); err != nil {
		*clientGone = true
		return
	}
	if hasFlusher {
		flusher.Flush()
	}
}

// sendSimpleError sends an error content chunk to the client.
func sendSimpleError(c *gin.Context, clientGone *bool, flusher http.Flusher, hasFlusher bool, msg string) {
	if *clientGone {
		return
	}
	chunk := map[string]interface{}{
		"choices": []map[string]interface{}{
			{
				"index":         0,
				"delta":         map[string]interface{}{"content": msg},
				"finish_reason": nil,
			},
		},
	}
	data, err := json.Marshal(chunk)
	if err != nil {
		return
	}
	if _, err := c.Writer.WriteString("data: " + string(data) + "\n"); err != nil {
		*clientGone = true
		return
	}
	if hasFlusher {
		flusher.Flush()
	}
}

// sendSimpleDone sends the [DONE] marker to the client.
func sendSimpleDone(c *gin.Context, clientGone *bool, flusher http.Flusher, hasFlusher bool) {
	if *clientGone {
		return
	}
	if _, err := c.Writer.WriteString("data: [DONE]\n"); err != nil {
		*clientGone = true
		return
	}
	if hasFlusher {
		flusher.Flush()
	}
}

// contextWithUserAndChat creates a context with user ID and chat ID for tool execution.
func contextWithUserAndChat(parent context.Context, userID, chatID string) context.Context {
	ctx := logger.WithUserID(parent, userID)
	ctx = logger.WithChatID(ctx, chatID)
	return ctx
}
