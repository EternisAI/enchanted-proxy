package mcp

import (
	"github.com/gin-gonic/gin"
	"github.com/mark3labs/mcp-go/server"
)

type Handler struct {
	service    *Service
	httpServer *server.StreamableHTTPServer
}

func NewHandler(service *Service) *Handler {
	streamServer := server.NewStreamableHTTPServer(service.GetMCPServer(),
		server.WithEndpointPath("/mcp"),
		server.WithStateLess(true),
	)

	return &Handler{
		service:    service,
		httpServer: streamServer,
	}
}

func (h *Handler) HandleMCPAny(c *gin.Context) {
	h.httpServer.ServeHTTP(c.Writer, c.Request)
}
