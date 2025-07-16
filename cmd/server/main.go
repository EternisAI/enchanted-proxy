package main

import (
	"context"
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
	"github.com/charmbracelet/log"
	"github.com/eternisai/enchanted-proxy/graph"
	"github.com/eternisai/enchanted-proxy/pkg/auth"
	"github.com/eternisai/enchanted-proxy/pkg/config"
	"github.com/eternisai/enchanted-proxy/pkg/telegram"
	"github.com/go-chi/chi/v5"
	"github.com/gorilla/websocket"
	"github.com/nats-io/nats.go"
	"github.com/pkg/errors"
	"github.com/rs/cors"
	"github.com/eternisai/enchanted-proxy/pkg/invitecode"
	"github.com/eternisai/enchanted-proxy/pkg/mcp"
	"github.com/eternisai/enchanted-proxy/pkg/oauth"
	"github.com/eternisai/enchanted-proxy/pkg/request_tracking"
	"github.com/eternisai/enchanted-proxy/pkg/storage/pg"
	"github.com/gin-gonic/gin"
)

func main() {
	logger := log.NewWithOptions(os.Stderr, log.Options{
		ReportCaller:    true,
		ReportTimestamp: true,
		Level:           log.DebugLevel,
		TimeFormat:      time.Kitchen,
	})

	config.LoadConfig()
	logger.Debug("Config loaded", "config", config.AppConfig)

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
	}

  var natsClient *nats.Conn
	if config.AppConfig.NatsURL != "" {
		nc, err := nats.Connect(config.AppConfig.NatsURL)
		if err != nil {
			logger.Warn("Failed to connect to NATS", "error", err, "url", config.AppConfig.NatsURL)
		} else {
			natsClient = nc
			logger.Info("Connected to NATS", "url", config.AppConfig.NatsURL)           
	// Initialize Telegram service if token is provided
	var telegramService *telegram.Service
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

graphqlPort := config.AppConfig.Port
	router := bootstrapGraphqlServer(graphqlServerInput{
		logger:          logger,
		port:            graphqlPort,
		natsClient:      natsClient,
		telegramService: telegramService,
	})
	logger.Info("Starting GraphQL HTTP server", "address", "http://localhost:"+graphqlPort)
	err = http.ListenAndServe(":"+graphqlPort, router)
	if err != nil && err != http.ErrServerClosed {
		logger.Error("HTTP server error", slog.Any("error", err))
		panic(errors.Wrap(err, "Unable to start server"))
	}
}

func gqlSchema(resolver *graph.Resolver) graphql.ExecutableSchema {
	config := graph.Config{
		Resolvers: resolver,
	}
	return graph.NewExecutableSchema(config)
}

func bootstrapGraphqlServer(input graphqlServerInput) *chi.Mux {
	router := chi.NewRouter()
	router.Use(cors.New(cors.Options{
		AllowCredentials: true,
		AllowedOrigins:   []string{"*"},
		AllowedHeaders:   []string{"Authorization", "Content-Type", "Accept"},
		Debug:            false,
	}).Handler)

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

type graphqlServerInput struct {
	logger          *log.Logger
	port            string
	natsClient      *nats.Conn
	telegramService *telegram.Service
}

// Legacy token validator setup - keeping for reference but not used in GraphQL
func NewTokenValidator(config *config.Config, logger *log.Logger) (auth.TokenValidator, error) {
	switch config.ValidatorType {
	case "firebase":
		return auth.NewFirebaseTokenValidator(context.Background(), config.FirebaseCredJSON)
	case "jwk":
		if config.JWTJWKSURL == "" {
			return nil, errors.New("JWT_JWKS_URL is required when using JWK validator")
		}
		return auth.NewTokenValidator(config.JWTJWKSURL)
	default:
		return nil, errors.New("invalid validator type: must be 'firebase' or 'jwk'")
	}
}
