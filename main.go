package main

import (
	"log"
	"oauth-proxy/config"
	"oauth-proxy/handlers"
	"oauth-proxy/services"

	"github.com/gin-gonic/gin"
)

func main() {
	// Load configuration
	config.LoadConfig()

	// Set Gin mode
	gin.SetMode(config.AppConfig.GinMode)

	// Initialize services
	oauthService := services.NewOAuthService()
	composioService := services.NewComposioService()

	// Initialize handlers
	oauthHandler := handlers.NewOAuthHandler(oauthService)
	composioHandler := handlers.NewComposioHandler(composioService)

	// Initialize Gin router
	router := gin.Default()

	// Add CORS middleware
	router.Use(func(c *gin.Context) {
		c.Header("Access-Control-Allow-Origin", "*")
		c.Header("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		c.Header("Access-Control-Allow-Headers", "Origin, Content-Type, Content-Length, Accept-Encoding, X-CSRF-Token, Authorization")

		if c.Request.Method == "OPTIONS" {
			c.AbortWithStatus(204)
			return
		}

		c.Next()
	})

	// Health check endpoint
	router.GET("/health", oauthHandler.HealthCheck)

	// OAuth API routes
	auth := router.Group("/auth")
	{
		auth.POST("/exchange", oauthHandler.ExchangeToken)
		auth.POST("/refresh", oauthHandler.RefreshToken)
	}

	// Compose API routes
	compose := router.Group("/composio")
	{
		compose.POST("/auth", composioHandler.CreateConnectedAccount)
		compose.GET("/account", composioHandler.GetConnectedAccount)
		compose.GET("/refresh", composioHandler.RefreshToken)
	}


	// Start server
	port := ":" + config.AppConfig.Port
	log.Printf("OAuth Proxy Server starting on port %s", config.AppConfig.Port)
	log.Printf("Supported platforms: Google, Slack, Twitter")
	log.Printf("Composio integration enabled")
	
	if err := router.Run(port); err != nil {
		log.Fatal("Failed to start server:", err)
	}
} 