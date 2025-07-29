package replicate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/sirupsen/logrus"
)

const (
	REPLICATE_GENERATE_IMAGE_TOOL_NAME = "generate_image"
)

const (
	REPLICATE_GENERATE_IMAGE_TOOL_DESCRIPTION = "Generate an image using the Flux model"
)

const (
	REPLICATE_API_URL = "https://api.replicate.com/v1"
)

type ReplicateGenerateImageArguments struct {
	Prompt       string  `json:"prompt" jsonschema:"required,description=Prompt for generated image"`
	Seed         *int    `json:"seed,omitempty" jsonschema:"description=Random seed for reproducible generation"`
	AspectRatio  *string `json:"aspect_ratio,omitempty" jsonschema:"description=Aspect ratio for the generated image,enum=1:1|16:9|21:9|3:2|2:3|4:5|5:4|3:4|4:3|9:16|9:21,default=1:1"`
	OutputFormat *string `json:"output_format,omitempty" jsonschema:"description=Format of the output images,enum=webp|jpg|png,default=webp"`
	NumOutputs   *int    `json:"num_outputs,omitempty" jsonschema:"description=Number of outputs to generate (1-4),default=1,minimum=1,maximum=4"`
}

type ReplicatePredictionInput struct {
	Prompt       string  `json:"prompt"`
	Seed         *int    `json:"seed,omitempty"`
	AspectRatio  *string `json:"aspect_ratio,omitempty"`
	OutputFormat *string `json:"output_format,omitempty"`
	NumOutputs   *int    `json:"num_outputs,omitempty"`
}

type ReplicatePredictionRequest struct {
	Version string                   `json:"version"`
	Input   ReplicatePredictionInput `json:"input"`
}

type ReplicatePredictionResponse struct {
	ID     string          `json:"id"`
	Status string          `json:"status"`
	Output json.RawMessage `json:"output"` // Can be a list of strings or other formats
	Error  interface{}     `json:"error"`
	Logs   string          `json:"logs"`
}

func ProcessReplicateGenerateImage(
	ctx context.Context,
	arguments ReplicateGenerateImageArguments,
	apiToken string,
) (*mcp.CallToolResult, error) {
	if arguments.Prompt == "" {
		return nil, errors.New("prompt is required")
	}

	if apiToken == "" {
		return nil, errors.New("replicate API token not configured")
	}

	model := "black-forest-labs/flux-schnell" // Default model

	input := ReplicatePredictionInput{
		Prompt: arguments.Prompt,
	}

	if arguments.Seed != nil {
		input.Seed = arguments.Seed
	}
	if arguments.AspectRatio != nil {
		input.AspectRatio = arguments.AspectRatio
	} else {
		input.AspectRatio = StringToPointer("1:1")
	}
	if arguments.OutputFormat != nil {
		input.OutputFormat = arguments.OutputFormat
	} else {
		input.OutputFormat = StringToPointer("webp")
	}
	if arguments.NumOutputs != nil {
		numOutputs := *arguments.NumOutputs
		if numOutputs < 1 {
			numOutputs = 1
		} else if numOutputs > 4 {
			numOutputs = 4
		}
		input.NumOutputs = &numOutputs
	}

	requestBody := ReplicatePredictionRequest{
		Version: model,
		Input:   input,
	}

	requestBodyBytes, err := json.Marshal(requestBody)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request body: %w", err)
	}

	// Create prediction
	createReq, err := http.NewRequestWithContext(ctx, "POST", REPLICATE_API_URL+"/predictions", bytes.NewBuffer(requestBodyBytes))
	if err != nil {
		return nil, fmt.Errorf("failed to create 'create prediction' request: %w", err)
	}
	createReq.Header.Set("Content-Type", "application/json")
	createReq.Header.Set("Authorization", "Token "+apiToken)

	client := &http.Client{}
	createResp, err := client.Do(createReq)
	if err != nil {
		return nil, fmt.Errorf("network error while creating prediction: %w", err)
	}
	defer func() {
		if err := createResp.Body.Close(); err != nil {
			logrus.Error("Error closing create prediction response body", "error", err)
		}
	}()

	if createResp.StatusCode >= 400 {
		errorText, _ := io.ReadAll(createResp.Body)
		return nil, fmt.Errorf("replicate API error (create prediction): %d %s\n%s", createResp.StatusCode, createResp.Status, errorText)
	}

	var predictionResp ReplicatePredictionResponse
	if err := json.NewDecoder(createResp.Body).Decode(&predictionResp); err != nil {
		return nil, fmt.Errorf("failed to parse JSON response from create prediction: %w", err)
	}

	predictionID := predictionResp.ID

	// Poll for completion
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err() // Respect context cancellation
		default:
			// Continue polling
		}

		getReq, err := http.NewRequestWithContext(ctx, "GET", REPLICATE_API_URL+"/predictions/"+predictionID, nil)
		if err != nil {
			return nil, fmt.Errorf("failed to create 'get prediction' request: %w", err)
		}
		getReq.Header.Set("Authorization", "Token "+apiToken)

		getResp, err := client.Do(getReq)
		if err != nil {
			return nil, fmt.Errorf("network error while getting prediction: %w", err)
		}

		if getResp.StatusCode >= 400 {
			errorText, _ := io.ReadAll(getResp.Body)
			_ = getResp.Body.Close() // Close body before returning error
			return nil, fmt.Errorf("replicate API error (get prediction): %d %s\n%s", getResp.StatusCode, getResp.Status, errorText)
		}

		var currentPrediction ReplicatePredictionResponse
		if err := json.NewDecoder(getResp.Body).Decode(&currentPrediction); err != nil {
			_ = getResp.Body.Close() // Close body before returning error
			return nil, fmt.Errorf("failed to parse JSON response from get prediction: %w", err)
		}
		_ = getResp.Body.Close() // Ensure body is closed after successful decode

		switch currentPrediction.Status {
		case "succeeded":
			var outputText string
			// Attempt to unmarshal as a list of strings (common for image models)
			var imageUrls []string
			if err := json.Unmarshal(currentPrediction.Output, &imageUrls); err == nil && len(imageUrls) > 0 {
				// If successful and has content, format as a list
				outputText = "Generated Images:\n"
				for i, url := range imageUrls {
					outputText += fmt.Sprintf("[%d] %s\n", i+1, url)
				}
			} else {
				// Fallback to stringifying the raw JSON if not a list of strings or empty
				outputText = string(currentPrediction.Output)
			}

			return mcp.NewToolResultText(outputText), nil
		case "failed":
			if currentPrediction.Error != nil {
				return nil, fmt.Errorf("image generation failed: %v. Logs: %s", currentPrediction.Error, currentPrediction.Logs)
			}
			return nil, fmt.Errorf("image generation failed. Logs: %s", currentPrediction.Logs)
		case "processing", "starting":
			// Continue polling
		default:
			// Unknown status, treat as an error or continue polling with caution
			logrus.Warn("Unknown prediction status from Replicate API", "status", currentPrediction.Status, "id", predictionID)
		}

		// Wait before polling again
		select {
		case <-time.After(1 * time.Second):
			// Wait successful
		case <-ctx.Done():
			return nil, ctx.Err() // Respect context cancellation during sleep
		}
	}
}

// Helper to convert string to pointer for optional fields.
func StringToPointer(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
