package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/charmbracelet/log"
	"github.com/eternisai/enchanted-proxy/pkg/auth"
	"github.com/eternisai/enchanted-proxy/pkg/composio"
	"github.com/eternisai/enchanted-proxy/pkg/config"
	"github.com/eternisai/enchanted-proxy/pkg/invitecode"
	"github.com/eternisai/enchanted-proxy/pkg/mcp"
	"github.com/eternisai/enchanted-proxy/pkg/oauth"
	"github.com/eternisai/enchanted-proxy/pkg/request_tracking"
	"github.com/eternisai/enchanted-proxy/pkg/storage/pg"
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

	// Initialize database.
	db, err := pg.InitDatabase(config.AppConfig.DatabaseURL)
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
	requestTrackingService := request_tracking.NewService(db.Queries, logger)
	mcpService := mcp.NewService()

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

	// All routes use Firebase/JWT auth
	router.Use(firebaseAuth.RequireAuth())

	router.Any("/mcp", mcpHandler.HandleMCPAny)

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
			rateLimit.GET("/status", request_tracking.RateLimitStatusHandler(requestTrackingService))
		}
	}

	// Protected proxy routes
	proxy := router.Group("/")
	proxy.Use(request_tracking.RequestTrackingMiddleware(requestTrackingService, logger))
	{
		proxy.POST("/chat/completions", proxyHandler)
		proxy.POST("/embeddings", proxyHandler)
		proxy.POST("/audio/speech", proxyHandler)
		proxy.POST("/audio/transcriptions", proxyHandler)
		proxy.POST("/audio/translations", proxyHandler)
	}

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

	srv := &http.Server{
		Addr:    port,
		Handler: router,
	}

	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Fatal("Failed to start server", "error", err)
		}
	}()

	// Graceful shutdown.
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	logger.Info("🛑 Shutting down server...")

	// Shutdown the request tracking service worker pool.
	requestTrackingService.Shutdown()
	logger.Info("✅ Request tracking service shutdown complete")

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(config.AppConfig.ServerShutdownTimeoutSeconds)*time.Second)
	defer cancel()

	if err := srv.Shutdown(ctx); err != nil {
		logger.Fatal("Server forced to shutdown", "error", err)
	}

	logger.Info("✅ Server exited")
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
