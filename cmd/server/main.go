package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/charmbracelet/log"
	"github.com/eternisai/enchanted-proxy/pkg/auth"
	"github.com/eternisai/enchanted-proxy/pkg/composio"
	"github.com/eternisai/enchanted-proxy/pkg/config"
	"github.com/eternisai/enchanted-proxy/pkg/invitecode"
	"github.com/eternisai/enchanted-proxy/pkg/mcp"
	"github.com/eternisai/enchanted-proxy/pkg/oauth"
	"github.com/eternisai/enchanted-proxy/pkg/request_tracking"
	"github.com/gin-gonic/gin"
)

var allowedBaseURLs = map[string]string{
	"https://openrouter.ai/api/v1":                  os.Getenv("OPENROUTER_API_KEY"),
	"https://api.openai.com/v1":                     os.Getenv("OPENAI_API_KEY"),
	"https://audio-processing.model.tinfoil.sh/v1/": os.Getenv("TINFOIL_API_KEY"),
}

func getAPIKey(baseURL string, config *config.Config) string {
	switch baseURL {
	case "https://openrouter.ai/api/v1":
		return config.OpenRouterAPIKey
	case "https://api.openai.com/v1":
		return config.OpenAIAPIKey
	case "https://audio-processing.model.tinfoil.sh/v1":
		return config.TinfoilAPIKey
	}
	return ""
}

func waHandler(c *gin.Context) {
	body, err := c.GetRawData()
	if err != nil {
		log.Printf("Error reading request body: %v", err)
		c.JSON(http.StatusBadRequest, gin.H{"status": false, "error": "Failed to read body"})
		return
	}

	log.Printf("WA Handler - Request body: %s", string(body))
	c.JSON(http.StatusOK, gin.H{"status": true})
}

// requestTrackingMiddleware logs requests for authenticated users and checks rate limits.
func requestTrackingMiddleware(trackingService *request_tracking.Service, logger *log.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		// Skip tracking for non-proxy endpoints
		if !isProxyEndpoint(c.Request.URL.Path) {
			c.Next()
			return
		}

		userID, exists := auth.GetUserUUID(c)
		if !exists {
			c.Next()
			return
		}

		if config.AppConfig.RateLimitEnabled {
			isUnderLimit, err := trackingService.CheckRateLimit(context.Background(), userID, config.AppConfig.RateLimitRequestsPerDay)
			if err != nil {
				logger.Error("Failed to check rate limit", "user_id", userID, "error", err)
			} else if !isUnderLimit {
				logger.Warn("🚨 RATE LIMIT EXCEEDED", "user_id", userID, "limit", config.AppConfig.RateLimitRequestsPerDay)

				if !config.AppConfig.RateLimitLogOnly {
					c.JSON(http.StatusTooManyRequests, gin.H{
						"error": "Rate limit exceeded. Please try again later.",
						"limit": config.AppConfig.RateLimitRequestsPerDay,
					})
					return
				}
			}
		}

		baseURL := c.GetHeader("X-BASE-URL")
		provider := request_tracking.GetProviderFromBaseURL(baseURL)

		go func() {
			info := request_tracking.RequestInfo{
				UserID:   userID,
				Endpoint: c.Request.URL.Path,
				Model:    "", // Not extracting model initially.
				Provider: provider,
			}

			if err := trackingService.LogRequest(c.Request.Context(), info); err != nil {
				logger.Error("Failed to log request", "error", err)
			}
		}()

		c.Next()
	}
}

// isProxyEndpoint checks if the request is for a proxy endpoint.
func isProxyEndpoint(path string) bool {
	proxyPaths := []string{
		"/chat/completions",
		"/embeddings",
		"/audio/speech",
		"/audio/transcriptions",
		"/audio/translations",
	}

	for _, pp := range proxyPaths {
		if path == pp {
			return true
		}
	}
	return false
}

// rateLimitStatusHandler returns the current rate limit status for the authenticated user.
func rateLimitStatusHandler(trackingService *request_tracking.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		userID, exists := auth.GetUserUUID(c)
		if !exists {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "User not authenticated"})
			return
		}

		if !config.AppConfig.RateLimitEnabled {
			c.JSON(http.StatusOK, gin.H{
				"enabled": false,
				"message": "Rate limiting is disabled",
			})
			return
		}

		isUnderLimit, err := trackingService.CheckRateLimit(c.Request.Context(), userID, config.AppConfig.RateLimitRequestsPerDay)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to check rate limit"})
			return
		}

		// Get current request count for the user in the last day
		oneDayAgo := time.Now().Add(-24 * time.Hour)
		requestCount, err := trackingService.GetUserRequestCountSince(c.Request.Context(), userID, oneDayAgo)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to get request count"})
			return
		}

		c.JSON(http.StatusOK, gin.H{
			"enabled":       config.AppConfig.RateLimitEnabled,
			"limit":         config.AppConfig.RateLimitRequestsPerDay,
			"current_count": requestCount,
			"remaining":     config.AppConfig.RateLimitRequestsPerDay - requestCount,
			"under_limit":   isUnderLimit,
			"log_only_mode": config.AppConfig.RateLimitLogOnly,
		})
	}
}

func main() {
	config.LoadConfig()

	logger := log.NewWithOptions(os.Stdout, log.Options{
		ReportCaller:    true,
		ReportTimestamp: true,
		Level:           log.DebugLevel,
		TimeFormat:      time.Kitchen,
	})

	// Set Gin mode
	logger.Info("Setting Gin mode", "mode", config.AppConfig.GinMode)
	gin.SetMode(config.AppConfig.GinMode)

	// Initialize database
	db, err := config.InitDatabase()
	if err != nil {
		logger.Fatal("Failed to initialize database", "error", err)
	}

	tokenValidator, err := NewTokenValidator(config.AppConfig, logger)
	if err != nil {
		logger.Fatal("Failed to initialize token validator", "error", err)
	}

	firebaseAuth, err := auth.NewFirebaseAuthMiddleware(tokenValidator)
	if err != nil {
		logger.Fatal("Failed to initialize Firebase auth middleware", "error", err)
	}

	// Initialize services
	oauthService := oauth.NewService()
	composioService := composio.NewService()
	inviteCodeService := invitecode.NewService(db.Queries)
	requestTrackingService := request_tracking.NewService(db.Queries)
	mcpService := mcp.NewService()

	// Start periodic materialized view refresh for request tracking.
	go func() {
		ticker := time.NewTicker(30 * time.Minute)
		defer ticker.Stop()
		logger.Info("🔄 Starting materialized view refresh routine (every 30 minutes)")

		for range ticker.C {
			if err := requestTrackingService.RefreshMaterializedView(context.Background()); err != nil {
				logger.Error("Failed to refresh materialized view", "error", err)
			} else {
				logger.Debug("📊 Materialized view refreshed successfully")
			}
		}
	}()

	// Initialize handlers
	oauthHandler := oauth.NewHandler(oauthService)
	composioHandler := composio.NewHandler(composioService)
	inviteCodeHandler := invitecode.NewHandler(inviteCodeService)
	mcpHandler := mcp.NewHandler(mcpService)

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

	// Debug/test endpoint (no auth required)
	router.POST("/wa", waHandler)

	// MCP API routes (uses Google OAuth validation)
	router.Any("/mcp", mcp.MCPAuthMiddleware(), mcpHandler.HandleMCPAny)

	// All other routes use Firebase/JWT auth
	router.Use(firebaseAuth.RequireAuth())

	// Add request tracking middleware after auth.
	router.Use(requestTrackingMiddleware(requestTrackingService, logger))

	// OAuth API routes
	auth := router.Group("/auth")
	{
		auth.POST("/exchange", oauthHandler.ExchangeToken)
		auth.POST("/refresh", oauthHandler.RefreshToken)
	}

	// Compose API routes (protected)
	compose := router.Group("/composio")
	{
		compose.POST("/auth", composioHandler.CreateConnectedAccount)
		compose.GET("/account", composioHandler.GetConnectedAccount)
		compose.GET("/refresh", composioHandler.RefreshToken)
	}

	// Invite code API routes (protected)
	api := router.Group("/api/v1")
	{
		invites := api.Group("/invites")
		{
			invites.GET("/:userID/whitelist", inviteCodeHandler.CheckUserWhitelist)
			invites.POST("/:code/redeem", inviteCodeHandler.RedeemInviteCode)
			invites.GET("/reset/:code", inviteCodeHandler.ResetInviteCode)
			invites.DELETE("/:id", inviteCodeHandler.DeleteInviteCode)
		}

		// Rate limiting routes (protected).
		// Not used yet.
		rateLimit := api.Group("/rate-limit")
		{
			rateLimit.GET("/status", rateLimitStatusHandler(requestTrackingService))
		}
	}

	// Protected proxy routes
	router.POST("/chat/completions", proxyHandler)
	router.POST("/embeddings", proxyHandler)
	router.POST("/audio/speech", proxyHandler)
	router.POST("/audio/transcriptions", proxyHandler)
	router.POST("/audio/translations", proxyHandler)

	port := ":" + config.AppConfig.Port

	logger.Info("🔁  proxy listening on " + port)
	logger.Info("✅  allowed base URLs", "paths", getKeys(allowedBaseURLs))

	// Log rate limiting configuration.
	if config.AppConfig.RateLimitEnabled {
		mode := "BLOCKING"
		if config.AppConfig.RateLimitLogOnly {
			mode = "LOG-ONLY"
		}
		logger.Info("🛡️  rate limiting enabled",
			"limit", config.AppConfig.RateLimitRequestsPerDay,
			"mode", mode)
	} else {
		logger.Info("⚠️  rate limiting disabled")
	}

	log.Fatal(router.Run(port))
}

// Helper function to get keys from map for logging.
func getKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

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

func NewTokenValidator(cfg *config.Config, logger *log.Logger) (auth.TokenValidator, error) {
	switch cfg.ValidatorType {
	case "firebase":
		if cfg.FirebaseProjectID == "" {
			logger.Error("firebase project ID is required")
			return nil, errors.New("firebase project ID is required")
		}

		logger.Info("creating Firebase token validator", "project_id", cfg.FirebaseProjectID)
		tokenValidator, err := auth.NewFirebaseTokenValidator(context.Background(), cfg.FirebaseCredJSON)
		if err != nil {
			logger.Error("Failed to create Firebase token validator", slog.Any("error", err))
			return nil, err
		}
		return tokenValidator, nil

	case "jwk":
		tokenValidator, err := auth.NewTokenValidator(cfg.JWTJWKSURL)
		if err != nil {
			logger.Error("Failed to create JWT token validator", slog.Any("error", err))
			return nil, err
		}
		return tokenValidator, nil

	default:
		logger.Error("Invalid validator type", "validator_type", cfg.ValidatorType)
		return nil, errors.New("validator type must be either 'firebase' or 'jwt'")
	}
}
