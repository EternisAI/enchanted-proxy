package mcp

import (
	"context"
	"fmt"

	"github.com/eternisai/enchanted-proxy/internal/config"
	"github.com/eternisai/enchanted-proxy/internal/mcp/perplexity"
	"github.com/eternisai/enchanted-proxy/internal/mcp/replicate"
	"github.com/eternisai/enchanted-proxy/internal/mcp/utils"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

type Service struct {
	mcpServer *server.MCPServer
}

func NewService() *Service {
	mcpServer := server.NewMCPServer("Enchanted MCP Server", "1.0.0")

	perplexitySchema, err := utils.ConverToInputSchema(perplexity.PerplexityAskArguments{})
	if err != nil {
		panic(fmt.Sprintf("Failed to convert PerplexityAskArguments to input schema: %v", err))
	}

	perplexityTool := mcp.NewToolWithRawSchema(perplexity.PERPLEXITY_ASK_TOOL_NAME, perplexity.PERPLEXITY_ASK_TOOL_DESCRIPTION, perplexitySchema)

	mcpServer.AddTool(perplexityTool, func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var args perplexity.PerplexityAskArguments
		if err := request.BindArguments(&args); err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("failed to bind arguments: %v", err)), nil
		}

		result, err := perplexity.ProcessPerplexityAsk(ctx, args, config.AppConfig.PerplexityAPIKey)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}

		return result, nil
	})

	replicateSchema, err := utils.ConverToInputSchema(replicate.ReplicateGenerateImageArguments{})
	if err != nil {
		panic(fmt.Sprintf("Failed to convert ReplicateGenerateImageArguments to input schema: %v", err))
	}

	replicateTool := mcp.NewToolWithRawSchema(replicate.REPLICATE_GENERATE_IMAGE_TOOL_NAME, replicate.REPLICATE_GENERATE_IMAGE_TOOL_DESCRIPTION, replicateSchema)

	mcpServer.AddTool(replicateTool, func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var args replicate.ReplicateGenerateImageArguments
		if err := request.BindArguments(&args); err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("failed to bind arguments: %v", err)), nil
		}

		result, err := replicate.ProcessReplicateGenerateImage(ctx, args, config.AppConfig.ReplicateAPIToken)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}

		return result, nil
	})

	return &Service{
		mcpServer: mcpServer,
	}
}

func (s *Service) GetMCPServer() *server.MCPServer {
	return s.mcpServer
}
