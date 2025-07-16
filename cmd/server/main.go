package main

import (
	"context"
	"net/http"
	"os"
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
	"github.com/eternisai/enchanted-proxy/pkg/storage/pg"
	"github.com/eternisai/enchanted-proxy/pkg/telegram"
	"github.com/go-chi/chi/v5"
	"github.com/gorilla/websocket"
	"github.com/nats-io/nats.go"
	"github.com/pkg/errors"
	"github.com/rs/cors"
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

	// Start GraphQL server
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
		logger.Fatal("HTTP server error", "error", err)
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
