package main

import (
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"oauth-proxy/config"
	"oauth-proxy/handlers"
	"oauth-proxy/services"
	"os"
	"strings"

	"github.com/gin-gonic/gin"
)

var allowedBaseURLs = map[string]string{
	"https://openrouter.ai/api/v1": os.Getenv("OPENROUTER_API_KEY"),
	"https://api.openai.com":       os.Getenv("OPENAI_API_KEY"),
}

func getAPIKey(baseURL string, config *config.Config) string {
	switch baseURL {
	case "https://openrouter.ai/api/":
		return config.OpenRouterAPIKey
	case "https://api.openai.com/":
		return config.OpenAIAPIKey
	}
	return ""
}

// Helper function to get keys from map for logging
func getKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

// Proxy handler function for Gin
func proxyHandler(c *gin.Context) {
	// Extract X-BASE-URL from header

	baseURL := c.GetHeader("X-BASE-URL")
	if baseURL == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "X-BASE-URL header is required"})
		return
	}

	// Check if base URL is in our allowed dictionary
	apiKey := getAPIKey(baseURL, config.AppConfig)
	if apiKey == "" {
		c.JSON(http.StatusForbidden, gin.H{"error": "Unauthorized base URL"})
		return
	}

	// Parse the target URL
	target, err := url.Parse(baseURL)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid URL format"})
		return
	}

	// Create reverse proxy for this specific target
	proxy := httputil.NewSingleHostReverseProxy(target)

	orig := proxy.Director
	proxy.Director = func(r *http.Request) {
		orig(r)
		log.Printf("🔁 Forwarding request to %s", target.String()+r.RequestURI)
		log.Printf("📤 Forwarding %s %s%s to %s", r.Method, r.Host, r.RequestURI, target.String()+r.RequestURI)

		r.Host = target.Host

		// Set Authorization header with Bearer token
		r.Header.Set("Authorization", "Bearer "+apiKey)

		// Handle User-Agent header
		if userAgent := r.Header.Get("User-Agent"); strings.Contains(userAgent, "OpenAI/Go") {
		} else {
			r.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")
		}

		// Clean up proxy headers
		r.Header.Del("X-Forwarded-For")
		r.Header.Del("X-Real-Ip")
		r.Header.Del("X-BASE-URL") // Remove our custom header before forwarding
	}

	proxy.ServeHTTP(c.Writer, c.Request)
}

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
		c.Header("Access-Control-Allow-Headers", "Origin, Content-Type, Content-Length, Accept-Encoding, X-CSRF-Token, Authorization, X-BASE-URL")

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

	// Proxy API routes - handles all HTTP methods
	router.POST("/v1/chat/completions", proxyHandler)

	// Start server
	port := ":" + config.AppConfig.Port
	log.Printf("OAuth Proxy Server starting on port %s", config.AppConfig.Port)
	log.Printf("Supported platforms: Google, Slack, Twitter")
	log.Printf("Composio integration enabled")
	log.Printf("🔁 Proxy enabled at /v1/chat/completions")
	log.Printf("✅ Allowed base URLs: %v", getKeys(allowedBaseURLs))

	if err := router.Run(port); err != nil {
		log.Fatal("Failed to start server:", err)
	}
}
