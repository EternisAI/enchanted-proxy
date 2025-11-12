package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
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
	"github.com/eternisai/enchanted-proxy/graph"
	"github.com/eternisai/enchanted-proxy/internal/auth"
	"github.com/eternisai/enchanted-proxy/internal/composio"
	"github.com/eternisai/enchanted-proxy/internal/config"
	"github.com/eternisai/enchanted-proxy/internal/deepr"
	"github.com/eternisai/enchanted-proxy/internal/iap"
	"github.com/eternisai/enchanted-proxy/internal/invitecode"
	"github.com/eternisai/enchanted-proxy/internal/keyshare"
	"github.com/eternisai/enchanted-proxy/internal/logger"
	"github.com/eternisai/enchanted-proxy/internal/mcp"
	"github.com/eternisai/enchanted-proxy/internal/messaging"
	"github.com/eternisai/enchanted-proxy/internal/oauth"
	"github.com/eternisai/enchanted-proxy/internal/proxy"
	"github.com/eternisai/enchanted-proxy/internal/request_tracking"
	"github.com/eternisai/enchanted-proxy/internal/search"
	"github.com/eternisai/enchanted-proxy/internal/storage/pg"
	"github.com/eternisai/enchanted-proxy/internal/task"
	"github.com/eternisai/enchanted-proxy/internal/telegram"
	"github.com/eternisai/enchanted-proxy/internal/title_generation"
	"github.com/gin-gonic/gin"
	"github.com/go-chi/chi/v5"
	"github.com/gorilla/websocket"
	"github.com/nats-io/nats.go"
	"github.com/rs/cors"
)

var allowedBaseURLs = map[string]string{
	"https://openrouter.ai/api/v1":     os.Getenv("OPENROUTER_API_KEY"),
	"https://api.openai.com/v1":        os.Getenv("OPENAI_API_KEY"),
	"https://inference.tinfoil.sh/v1/": os.Getenv("TINFOIL_API_KEY"),
	"https://cloud-api.near.ai/v1":     os.Getenv("NEAR_API_KEY"),
	"http://127.0.0.1:20001/v1":        os.Getenv("ETERNIS_INFERENCE_API_KEY"),
}

func waHandler(logger *logger.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		log := logger.WithContext(c.Request.Context()).WithComponent("wa_handler")

		body, err := c.GetRawData()
		if err != nil {
			log.Error("failed to read request body", slog.String("error", err.Error()))
			c.JSON(http.StatusBadRequest, gin.H{"status": false, "error": "Failed to read body"})
			return
		}

		log.Debug("wa handler request received", slog.String("body", string(body)))
		c.JSON(http.StatusOK, gin.H{"status": true})
	}
}

func main() {
	config.LoadConfig()

	loggerConfig := logger.FromConfig(config.AppConfig.LogLevel, config.AppConfig.LogFormat)
	logger := logger.New(loggerConfig)
	log := logger.WithComponent("main")

	log.Info("logger initialized",
		slog.String("log_level", config.AppConfig.LogLevel),
		slog.String("log_format", config.AppConfig.LogFormat),
		slog.String("effective_level", loggerConfig.Level.String()),
	)

	// Set Gin mode
	log.Info("setting gin mode", slog.String("mode", config.AppConfig.GinMode))
	gin.SetMode(config.AppConfig.GinMode)

	// Initialize database
	log.Info("initializing database connection")
	db, err := pg.InitDatabase(config.AppConfig.DatabaseURL)
	if err != nil {
		log.Error("failed to initialize database", slog.String("error", err.Error()))
		os.Exit(1)
	}
	log.Info("database connection established")

	tokenValidator, err := NewTokenValidator(config.AppConfig, logger)
	if err != nil {
		log.Error("failed to initialize token validator", slog.String("error", err.Error()))
		os.Exit(1)
	}

	firebaseAuth, err := auth.NewFirebaseAuthMiddleware(tokenValidator)
	if err != nil {
		log.Error("failed to initialize firebase auth middleware", slog.String("error", err.Error()))
		os.Exit(1)
	}

	// Initialize Firebase client for Firestore (used for deep research tracking)
	var firebaseClient *auth.FirebaseClient

	if config.AppConfig.FirebaseCredJSON != "" {
		firebaseClient, err = auth.NewFirebaseClient(context.Background(), config.AppConfig.FirebaseProjectID, config.AppConfig.FirebaseCredJSON)
		if err != nil {
			log.Error("failed to initialize firebase client", slog.String("error", err.Error()))
			os.Exit(1)
		}
		log.Info("firebase client initialized")

		// Ensure cleanup on shutdown
		defer func() {
			if err := firebaseClient.Close(); err != nil {
				log.Error("failed to close firebase client", slog.String("error", err.Error()))
			}
		}()
	} else {
		log.Warn("firebase credentials not provided - deep research tracking will not work properly")
	}

	// Initialize services
	oauthService := oauth.NewService(logger.WithComponent("oauth"))
	composioService := composio.NewService(logger.WithComponent("composio"))
	inviteCodeService := invitecode.NewService(db.Queries)
	requestTrackingService := request_tracking.NewService(db.Queries, logger.WithComponent("request_tracking"))
	iapService := iap.NewService(db.Queries)
	mcpService := mcp.NewService()
	searchService := search.NewService(logger.WithComponent("search"))

	taskService, err := task.NewService(
		config.AppConfig.TemporalEndpoint,
		config.AppConfig.TemporalNamespace,
		config.AppConfig.TemporalAPIKey,
		db.Queries,
		logger.WithComponent("task"),
	)
	if err != nil {
		log.Error("failed to initialize task service", slog.String("error", err.Error()))
		os.Exit(1)
	}

	// Initialize deep research storage
	deeprStorage := deepr.NewDBStorage(logger.WithComponent("deepr-storage"), db.DB)
	deeprSessionManager := deepr.NewSessionManager(logger.WithComponent("deepr-session"))

	// Initialize message storage service
	var messageService *messaging.Service
	if config.AppConfig.MessageStorageEnabled && firebaseClient != nil {
		// Access Firestore client from FirebaseClient
		messageService = messaging.NewService(firebaseClient.GetFirestoreClient(), logger.WithComponent("messaging"))
		log.Info("message storage service initialized")

		// Ensure cleanup on shutdown
		defer messageService.Shutdown()
	} else {
		if !config.AppConfig.MessageStorageEnabled {
			log.Info("message storage disabled by configuration")
		} else {
			log.Warn("firebase client not available - message storage will not work")
		}
	}

	// Initialize title generation service
	var titleService *title_generation.Service
	if config.AppConfig.MessageStorageEnabled && messageService != nil && firebaseClient != nil {
		titleService = title_generation.NewService(
			logger.WithComponent("title_generation"),
			messageService,
			messaging.NewFirestoreClient(firebaseClient.GetFirestoreClient()),
		)
		log.Info("title generation service initialized")

		// Ensure cleanup on shutdown
		defer titleService.Shutdown()
	} else {
		log.Info("title generation service disabled (requires message storage)")
	}

	// Initialize key sharing service
	var keyshareHandler *keyshare.Handler
	if firebaseClient != nil {
		keyshareWSManager := keyshare.NewWebSocketManager(logger.WithComponent("keyshare-ws"))
		keyshareFirestore := keyshare.NewFirestoreClient(firebaseClient.GetFirestoreClient())
		keyshareService := keyshare.NewService(keyshareFirestore, keyshareWSManager, logger.WithComponent("keyshare"))
		keyshareHandler = keyshare.NewHandler(keyshareService, keyshareWSManager, logger.WithComponent("keyshare"))
		log.Info("key sharing service initialized")

		// Start cleanup job for expired sessions
		go func() {
			cleanupTicker := time.NewTicker(5 * time.Minute)
			defer cleanupTicker.Stop()

			for range cleanupTicker.C {
				ctx := context.Background()
				deleted, err := keyshareService.CleanupExpiredSessions(ctx)
				if err != nil {
					log.Error("key share cleanup job failed", slog.String("error", err.Error()))
				} else if deleted > 0 {
					log.Info("key share cleanup job completed", slog.Int("deleted", deleted))
				}
			}
		}()
	} else {
		log.Info("key sharing service disabled (requires firebase client)")
	}

	// Initialize handlers
	oauthHandler := oauth.NewHandler(oauthService, logger.WithComponent("oauth"))
	composioHandler := composio.NewHandler(composioService, logger.WithComponent("composio"))
	inviteCodeHandler := invitecode.NewHandler(inviteCodeService)
	iapHandler := iap.NewHandler(iapService, logger.WithComponent("iap"))
	mcpHandler := mcp.NewHandler(mcpService)
	searchHandler := search.NewHandler(searchService, logger.WithComponent("search"))
	taskHandler := task.NewHandler(taskService, logger.WithComponent("task"))

	// Initialize NATS for Telegram
	var natsClient *nats.Conn
	if config.AppConfig.NatsURL != "" {
		nc, err := nats.Connect(config.AppConfig.NatsURL)
		if err != nil {
			log.Warn("failed to connect to nats", slog.String("error", err.Error()), slog.String("url", config.AppConfig.NatsURL))
		} else {
			natsClient = nc
			log.Info("connected to nats", slog.String("url", config.AppConfig.NatsURL))
		}
	}

	// Initialize Telegram service if token is provided
	var telegramService *telegram.Service
	if config.AppConfig.EnableTelegramServer {
		if config.AppConfig.TelegramToken != "" {
			telegramInput := telegram.TelegramServiceInput{
				Logger:     logger.WithComponent("telegram"),
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
					log.Error("telegram service failed", slog.String("error", err.Error()))
				}
			}()

			log.Info("telegram service initialized and started")
		} else {
			log.Warn("no telegram token provided, telegram service disabled")
		}
	} else {
		log.Info("telegram service disabled")
	}

	// Initialize REST API router (original proxy functionality)
	router := setupRESTServer(restServerInput{
		logger:                 logger,
		firebaseAuth:           firebaseAuth,
		firebaseClient:         firebaseClient,
		requestTrackingService: requestTrackingService,
		messageService:         messageService,
		titleService:           titleService,
		oauthHandler:           oauthHandler,
		composioHandler:        composioHandler,
		inviteCodeHandler:      inviteCodeHandler,
		iapHandler:             iapHandler,
		mcpHandler:             mcpHandler,
		searchHandler:          searchHandler,
		taskHandler:            taskHandler,
		keyshareHandler:        keyshareHandler,
		deeprStorage:           deeprStorage,
		deeprSessionManager:    deeprSessionManager,
		config:                 config.AppConfig,
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
			log.Info("starting graphql server for telegram", slog.String("port", "8081"))
			if err := graphqlServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				log.Error("graphql server error", slog.String("error", err.Error()))
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
		log.Info("proxy listening", slog.String("port", restPort))
		log.Info("allowed base urls configured", slog.Any("paths", getKeys(allowedBaseURLs)))

		// Log rate limiting configuration
		if config.AppConfig.RateLimitEnabled {
			mode := "BLOCKING"
			if config.AppConfig.RateLimitLogOnly {
				mode = "LOG-ONLY"
			}
			log.Info("rate limiting enabled",
				slog.String("mode", mode))
		} else {
			log.Info("rate limiting disabled")
		}

		if err := restServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Error("rest server error", slog.String("error", err.Error()))
			os.Exit(1)
		}
	}()

	// Graceful shutdown
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	log.Info("shutting down servers")

	// Shutdown the request tracking service worker pool
	requestTrackingService.Shutdown()
	log.Info("request tracking service shutdown complete")

	// Shutdown the task service (close Temporal client)
	if taskService != nil {
		taskService.Close()
		log.Info("task service shutdown complete")
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(config.AppConfig.ServerShutdownTimeoutSeconds)*time.Second)
	defer cancel()

	// Shutdown both servers
	if err := restServer.Shutdown(ctx); err != nil {
		log.Error("rest server forced to shutdown", slog.String("error", err.Error()))
	}
	if graphqlServer != nil {
		if err := graphqlServer.Shutdown(ctx); err != nil {
			log.Error("graphql server forced to shutdown", slog.String("error", err.Error()))
		}
	}

	log.Info("servers exited")
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
	logger                 *logger.Logger
	firebaseAuth           *auth.FirebaseAuthMiddleware
	firebaseClient         *auth.FirebaseClient
	requestTrackingService *request_tracking.Service
	messageService         *messaging.Service
	titleService           *title_generation.Service
	oauthHandler           *oauth.Handler
	composioHandler        *composio.Handler
	inviteCodeHandler      *invitecode.Handler
	iapHandler             *iap.Handler
	mcpHandler             *mcp.Handler
	searchHandler          *search.Handler
	taskHandler            *task.Handler
	keyshareHandler        *keyshare.Handler
	deeprStorage           deepr.MessageStorage
	deeprSessionManager    *deepr.SessionManager
	config                 *config.Config
}

func setupRESTServer(input restServerInput) *gin.Engine {
	router := gin.New()
	router.Use(gin.Recovery())

	// Add request logging middleware.
	router.Use(logger.RequestLoggingMiddleware(input.logger))

	// Add CORS middleware
	router.Use(func(c *gin.Context) {
		c.Header("Access-Control-Allow-Origin", "*")
		c.Header("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		c.Header("Access-Control-Allow-Headers", "Origin, Content-Type, Content-Length, Accept-Encoding, X-CSRF-Token, Authorization, X-BASE-URL, X-Client-Platform, X-Chat-ID, X-Message-ID, X-User-Message-ID, X-Encryption-Enabled")

		if c.Request.Method == "OPTIONS" {
			c.AbortWithStatus(204)
			return
		}

		c.Next()
	})

	// Debug/test endpoint (no auth required)
	router.POST("/wa", waHandler(input.logger))

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
			rateLimit.GET("/status", request_tracking.RateLimitStatusHandler(input.requestTrackingService, input.logger))
		}

		// IAP (protected)
		sub := api.Group("/subscription")
		{
			sub.POST("/appstore/attach", input.iapHandler.AttachAppStoreSubscription)
		}

		// Search API routes (protected)
		api.POST("/search", input.searchHandler.PostSearchHandler)        // POST /api/v1/search (SerpAPI)
		api.POST("/exa/search", input.searchHandler.PostExaSearchHandler) // POST /api/v1/exa/search (Exa AI)

		// Task API routes (protected)
		tasks := api.Group("/tasks")
		{
			tasks.POST("", input.taskHandler.CreateTask)           // POST /api/v1/tasks - Create a new task
			tasks.GET("", input.taskHandler.GetTasks)              // GET /api/v1/tasks - Get all tasks for user
			tasks.DELETE("/:taskId", input.taskHandler.DeleteTask) // DELETE /api/v1/tasks/:taskId - Delete a task
		}

		// Deep Research endpoints (protected)
		api.POST("/deepresearch/start", deepr.StartDeepResearchHandler(input.logger, input.requestTrackingService, input.firebaseClient, input.deeprStorage, input.deeprSessionManager, input.config.DeepResearchRateLimitEnabled)) // POST API to start deep research

		// Key Sharing API routes (protected)
		if input.keyshareHandler != nil {
			encryption := api.Group("/encryption")
			{
				keyShare := encryption.Group("/key-share")
				{
					api.POST("/deepresearch/clarify", deepr.ClarifyDeepResearchHandler(input.logger, input.deeprSessionManager))                                                                                                       // POST API to submit clarification response
					api.GET("/deepresearch/ws", deepr.DeepResearchHandler(input.logger, input.requestTrackingService, input.firebaseClient, input.deeprStorage, input.deeprSessionManager, input.config.DeepResearchRateLimitEnabled)) // WebSocket proxy for deep research
					keyShare.POST("/session", input.keyshareHandler.CreateSession)                                                                                                                                                     // POST /api/v1/encryption/key-share/session
					keyShare.POST("/session/:sessionId", input.keyshareHandler.SubmitKey)                                                                                                                                              // POST /api/v1/encryption/key-share/session/:sessionId
					keyShare.GET("/session/:sessionId/listen", input.keyshareHandler.WebSocketListen)                                                                                                                                  // WebSocket /api/v1/encryption/key-share/session/:sessionId/listen
				}
			}
		}
	}

	// Protected proxy routes
	proxyGroup := router.Group("/")
	proxyGroup.Use(request_tracking.RequestTrackingMiddleware(input.requestTrackingService, input.logger))
	{
		// AI service endpoints
		proxyGroup.POST("/chat/completions", proxy.ProxyHandler(input.logger, input.requestTrackingService, input.messageService, input.titleService))
		proxyGroup.POST("/responses", proxy.ProxyHandler(input.logger, input.requestTrackingService, input.messageService, input.titleService))
		proxyGroup.GET("/responses/:responseId", proxy.ProxyHandler(input.logger, input.requestTrackingService, input.messageService, input.titleService))
		proxyGroup.POST("/embeddings", proxy.ProxyHandler(input.logger, input.requestTrackingService, input.messageService, input.titleService))
		proxyGroup.POST("/audio/speech", proxy.ProxyHandler(input.logger, input.requestTrackingService, input.messageService, input.titleService))
		proxyGroup.POST("/audio/transcriptions", proxy.ProxyHandler(input.logger, input.requestTrackingService, input.messageService, input.titleService))
		proxyGroup.POST("/audio/translations", proxy.ProxyHandler(input.logger, input.requestTrackingService, input.messageService, input.titleService))
	}

	return router
}

type graphqlServerInput struct {
	logger          *logger.Logger
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
			input.logger.WithComponent("graphql").Error(
				"graphql operation error",
				slog.String("operation_name", oc.OperationName),
				slog.String("raw_query", oc.RawQuery),
				slog.Any("variables", oc.Variables),
				slog.Any("errors", resp.Errors),
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

func NewTokenValidator(cfg *config.Config, logger *logger.Logger) (auth.TokenValidator, error) {
	log := logger.WithComponent("auth")

	switch cfg.ValidatorType {
	case "firebase":
		if cfg.FirebaseProjectID == "" {
			log.Error("firebase project id is required")
			return nil, errors.New("firebase project ID is required")
		}

		log.Info("creating firebase token validator", slog.String("project_id", cfg.FirebaseProjectID))
		tokenValidator, err := auth.NewFirebaseTokenValidator(context.Background(), cfg.FirebaseCredJSON)
		if err != nil {
			log.Error("failed to create firebase token validator", slog.String("error", err.Error()))
			return nil, err
		}
		return tokenValidator, nil

	case "jwk":
		tokenValidator, err := auth.NewTokenValidator(cfg.JWTJWKSURL)
		if err != nil {
			log.Error("failed to create jwt token validator", slog.String("error", err.Error()))
			return nil, err
		}
		return tokenValidator, nil

	default:
		log.Error("invalid validator type", slog.String("validator_type", cfg.ValidatorType))
		return nil, errors.New("validator type must be either 'firebase' or 'jwt'")
	}
}
