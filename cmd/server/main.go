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

	"github.com/99designs/gqlgen/graphql"
	"github.com/99designs/gqlgen/graphql/handler"
	"github.com/99designs/gqlgen/graphql/handler/extension"
	"github.com/99designs/gqlgen/graphql/handler/transport"
	"github.com/99designs/gqlgen/graphql/playground"
	"github.com/charmbracelet/log"
	"github.com/eternisai/enchanted-proxy/graph"
	"github.com/eternisai/enchanted-proxy/pkg/auth"
	"github.com/eternisai/enchanted-proxy/pkg/composio"
	"github.com/eternisai/enchanted-proxy/pkg/config"
	"github.com/eternisai/enchanted-proxy/pkg/invitecode"
	"github.com/eternisai/enchanted-proxy/pkg/mcp"
	"github.com/eternisai/enchanted-proxy/pkg/oauth"
	"github.com/eternisai/enchanted-proxy/pkg/request_tracking"
	"github.com/eternisai/enchanted-proxy/pkg/storage/pg"
	"github.com/eternisai/enchanted-proxy/pkg/telegram"
	"github.com/gin-gonic/gin"
	"github.com/go-chi/chi/v5"
	"github.com/gorilla/websocket"
	"github.com/nats-io/nats.go"
	"github.com/rs/cors"
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

	// Initialize database
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

	// Initialize NATS for Telegram
	var natsClient *nats.Conn
	if config.AppConfig.NatsURL != "" {
		nc, err := nats.Connect(config.AppConfig.NatsURL)
		if err != nil {
			logger.Warn("Failed to connect to NATS", "error", err, "url", config.AppConfig.NatsURL)
		} else {
			natsClient = nc
			logger.Info("Connected to NATS", "url", config.AppConfig.NatsURL)
		}
	}

	// Initialize Telegram service if token is provided
	var telegramService *telegram.Service
	if config.AppConfig.EnableTelegramServer {
		if config.AppConfig.TelegramToken != "" {
			telegramInput := telegram.TelegramServiceInput{
				Logger:     logger,
				Token:      config.AppConfig.TelegramToken,
				Store:      db,
				Queries:    db.Queries,
				NatsClient: natsClient,
			}
			telegramService = telegram.NewService(telegramInput)

			// Start Telegram polling in background
			go func() {
				ctx := context.Background()
				if err := telegramService.Start(ctx); err != nil {
					logger.Error("Telegram service failed", "error", err)
				}
			}()

			logger.Info("Telegram service initialized and started")
		} else {
			logger.Warn("No Telegram token provided, Telegram service disabled")
		}
	} else {
		logger.Info("Telegram service disabled")
	}

	// Initialize REST API router (original proxy functionality)
	router := setupRESTServer(restServerInput{
		logger:                 logger,
		firebaseAuth:           firebaseAuth,
		requestTrackingService: requestTrackingService,
		oauthHandler:           oauthHandler,
		composioHandler:        composioHandler,
		inviteCodeHandler:      inviteCodeHandler,
		mcpHandler:             mcpHandler,
	})

	// Initialize GraphQL server for Telegram
	var graphqlServer *http.Server
	if telegramService != nil {
		graphqlRouter := setupGraphQLServer(graphqlServerInput{
			logger:          logger,
			natsClient:      natsClient,
			telegramService: telegramService,
			firebaseAuth:    firebaseAuth,
		})

		graphqlServer = &http.Server{
			Addr:    ":8081",
			Handler: graphqlRouter,
		}

		go func() {
			logger.Info("Starting GraphQL server for Telegram", "port", "8081")
			if err := graphqlServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				logger.Error("GraphQL server error", "error", err)
			}
		}()
	}

	// Start main REST API server
	restPort := ":" + config.AppConfig.Port
	restServer := &http.Server{
		Addr:    restPort,
		Handler: router,
	}

	go func() {
		logger.Info("🔁 proxy listening on " + restPort)
		logger.Info("✅ allowed base URLs", "paths", getKeys(allowedBaseURLs))

		// Log rate limiting configuration
		if config.AppConfig.RateLimitEnabled {
			mode := "BLOCKING"
			if config.AppConfig.RateLimitLogOnly {
				mode = "LOG-ONLY"
			}
			logger.Info("🛡️ rate limiting enabled",
				"limit", config.AppConfig.RateLimitRequestsPerDay,
				"mode", mode)
		} else {
			logger.Info("⚠️ rate limiting disabled")
		}

		if err := restServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Fatal("REST server error", "error", err)
		}
	}()

	// Graceful shutdown
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	logger.Info("🛑 Shutting down servers...")

	// Shutdown the request tracking service worker pool
	requestTrackingService.Shutdown()
	logger.Info("✅ Request tracking service shutdown complete")

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(config.AppConfig.ServerShutdownTimeoutSeconds)*time.Second)
	defer cancel()

	// Shutdown both servers
	if err := restServer.Shutdown(ctx); err != nil {
		logger.Error("REST server forced to shutdown", "error", err)
	}
	if graphqlServer != nil {
		if err := graphqlServer.Shutdown(ctx); err != nil {
			logger.Error("GraphQL server forced to shutdown", "error", err)
		}
	}

	logger.Info("✅ Servers exited")
}

// Helper function to get keys from map for logging.
func getKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

type restServerInput struct {
	logger                 *log.Logger
	firebaseAuth           *auth.FirebaseAuthMiddleware
	requestTrackingService *request_tracking.Service
	oauthHandler           *oauth.Handler
	composioHandler        *composio.Handler
	inviteCodeHandler      *invitecode.Handler
	mcpHandler             *mcp.Handler
}

func setupRESTServer(input restServerInput) *gin.Engine {
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
	router.Use(input.firebaseAuth.RequireAuth())

	router.Any("/mcp", input.mcpHandler.HandleMCPAny)

	// OAuth API routes
	auth := router.Group("/auth")
	{
		auth.POST("/exchange", input.oauthHandler.ExchangeToken)
		auth.POST("/refresh", input.oauthHandler.RefreshToken)
	}

	// Composio API routes (protected)
	compose := router.Group("/composio")
	{
		compose.POST("/auth", input.composioHandler.CreateConnectedAccount)
		compose.GET("/account", input.composioHandler.GetConnectedAccount)
		compose.GET("/refresh", input.composioHandler.RefreshToken)
	}

	// Invite code API routes (protected)
	api := router.Group("/api/v1")
	{
		invites := api.Group("/invites")
		{
			invites.GET("/:userID/whitelist", input.inviteCodeHandler.CheckUserWhitelist)
			invites.POST("/:code/redeem", input.inviteCodeHandler.RedeemInviteCode)
			invites.GET("/reset/:code", input.inviteCodeHandler.ResetInviteCode)
			invites.DELETE("/:id", input.inviteCodeHandler.DeleteInviteCode)
		}

		// Rate limiting routes (protected)
		rateLimit := api.Group("/rate-limit")
		{
			rateLimit.GET("/status", request_tracking.RateLimitStatusHandler(input.requestTrackingService))
		}
	}

	// Protected proxy routes
	proxy := router.Group("/")
	proxy.Use(request_tracking.RequestTrackingMiddleware(input.requestTrackingService, input.logger))
	{
		proxy.POST("/chat/completions", proxyHandler)
		proxy.POST("/embeddings", proxyHandler)
		proxy.POST("/audio/speech", proxyHandler)
		proxy.POST("/audio/transcriptions", proxyHandler)
		proxy.POST("/audio/translations", proxyHandler)
	}

	return router
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

type graphqlServerInput struct {
	logger          *log.Logger
	natsClient      *nats.Conn
	telegramService *telegram.Service
	firebaseAuth    *auth.FirebaseAuthMiddleware
}

func setupGraphQLServer(input graphqlServerInput) *chi.Mux {
	router := chi.NewRouter()

	// Configure CORS with configurable origins
	allowedOrigins := []string{"http://localhost:3000"} // Default for development
	if config.AppConfig.CORSAllowedOrigins != "" {
		// Split comma-separated origins from environment variable
		origins := strings.Split(config.AppConfig.CORSAllowedOrigins, ",")
		for i, origin := range origins {
			origins[i] = strings.TrimSpace(origin)
		}
		allowedOrigins = origins
	}

	router.Use(cors.New(cors.Options{
		AllowCredentials: true,
		AllowedOrigins:   allowedOrigins,
		AllowedHeaders:   []string{"Authorization", "Content-Type", "Accept"},
		AllowedMethods:   []string{"GET", "POST", "OPTIONS"},
		Debug:            false,
	}).Handler)

	// Add authentication middleware to protect all GraphQL endpoints
	// TEMPORARILY DISABLED FOR DEBUGGING WEBSOCKET SUBSCRIPTIONS
	// router.Use(input.firebaseAuth.RequireAuthHTTP())

	// Create the GraphQL resolver with dependencies
	resolver := &graph.Resolver{
		Logger:          input.logger,
		TelegramService: input.telegramService,
		NatsClient:      input.natsClient,
	}

	srv := handler.New(gqlSchema(resolver))
	srv.AddTransport(transport.SSE{})
	srv.AddTransport(transport.POST{})
	srv.AddTransport(transport.Options{})
	srv.AddTransport(transport.GET{})

	srv.AddTransport(transport.Websocket{
		KeepAlivePingInterval: 10 * time.Second,
		Upgrader: websocket.Upgrader{
			CheckOrigin: func(r *http.Request) bool {
				return true
			},
			ReadBufferSize:  1024,
			WriteBufferSize: 1024,
		},
	})

	srv.Use(extension.Introspection{})
	srv.AroundResponses(func(ctx context.Context, next graphql.ResponseHandler) *graphql.Response {
		resp := next(ctx)

		if resp != nil && resp.Errors != nil && len(resp.Errors) > 0 {
			oc := graphql.GetOperationContext(ctx)
			input.logger.Error(
				"gql error",
				"operation_name",
				oc.OperationName,
				"raw_query",
				oc.RawQuery,
				"variables",
				oc.Variables,
				"errors",
				resp.Errors,
			)
		}

		return resp
	})

	router.Handle("/", playground.Handler("GraphQL playground", "/query"))
	router.Handle("/query", srv)

	return router
}

func gqlSchema(resolver *graph.Resolver) graphql.ExecutableSchema {
	config := graph.Config{
		Resolvers: resolver,
	}
	return graph.NewExecutableSchema(config)
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
