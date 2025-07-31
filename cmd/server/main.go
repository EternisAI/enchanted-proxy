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
	"github.com/eternisai/enchanted-proxy/internal/invitecode"
	"github.com/eternisai/enchanted-proxy/internal/logger"
	"github.com/eternisai/enchanted-proxy/internal/mcp"
	"github.com/eternisai/enchanted-proxy/internal/oauth"
	"github.com/eternisai/enchanted-proxy/internal/proxy"
	"github.com/eternisai/enchanted-proxy/internal/request_tracking"
	"github.com/eternisai/enchanted-proxy/internal/storage/pg"
	"github.com/eternisai/enchanted-proxy/internal/telegram"
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

	// Initialize services
	oauthService := oauth.NewService(logger.WithComponent("oauth"))
	composioService := composio.NewService(logger.WithComponent("composio"))
	inviteCodeService := invitecode.NewService(db.Queries)
	requestTrackingService := request_tracking.NewService(db.Queries, logger.WithComponent("request_tracking"))
	mcpService := mcp.NewService()

	// Initialize handlers
	oauthHandler := oauth.NewHandler(oauthService, logger.WithComponent("oauth"))
	composioHandler := composio.NewHandler(composioService, logger.WithComponent("composio"))
	inviteCodeHandler := invitecode.NewHandler(inviteCodeService)
	mcpHandler := mcp.NewHandler(mcpService)

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
				slog.Int64("limit", config.AppConfig.RateLimitRequestsPerDay),
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
	requestTrackingService *request_tracking.Service
	oauthHandler           *oauth.Handler
	composioHandler        *composio.Handler
	inviteCodeHandler      *invitecode.Handler
	mcpHandler             *mcp.Handler
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
		c.Header("Access-Control-Allow-Headers", "Origin, Content-Type, Content-Length, Accept-Encoding, X-CSRF-Token, Authorization, X-BASE-URL")

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
			rateLimit.GET("/status", request_tracking.RateLimitStatusHandler(input.requestTrackingService))
		}
	}

	// Protected proxy routes
	proxyGroup := router.Group("/")
	proxyGroup.Use(request_tracking.RequestTrackingMiddleware(input.requestTrackingService, input.logger))
	{
		proxyGroup.POST("/chat/completions", proxy.ProxyHandler(input.logger))
		proxyGroup.POST("/embeddings", proxy.ProxyHandler(input.logger))
		proxyGroup.POST("/audio/speech", proxy.ProxyHandler(input.logger))
		proxyGroup.POST("/audio/transcriptions", proxy.ProxyHandler(input.logger))
		proxyGroup.POST("/audio/translations", proxy.ProxyHandler(input.logger))
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
