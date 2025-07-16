package config

import (
	"log"
	"os"
	"strconv"

	"github.com/joho/godotenv"
)

type Config struct {
	Port                  string
	GinMode               string
	FirebaseProjectID     string
	DatabaseURL           string
	GoogleClientID        string
	GoogleClientSecret    string
	SlackClientID         string
	SlackClientSecret     string
	TwitterClientID       string
	TwitterClientSecret   string
	ComposioAPIKey        string
	ComposioTwitterConfig string
	OpenAIAPIKey          string
	OpenRouterAPIKey      string
	TinfoilAPIKey         string
	ValidatorType         string // "jwk" or "firebase"
	JWTJWKSURL            string
	FirebaseCredJSON      string

	// MCP
	PerplexityAPIKey  string
	ReplicateAPIToken string

	// Rate Limiting
	RateLimitEnabled        bool
	RateLimitRequestsPerDay int64
	RateLimitLogOnly        bool // If true, only log violations, don't block.

	// Telegram
	TelegramToken string
	NatsURL       string

	// Database Connection Pool
	DBMaxOpenConns    int
	DBMaxIdleConns    int
	DBConnMaxIdleTime int // in minutes
	DBConnMaxLifetime int // in minutes

	// Worker Pool
	RequestTrackingWorkerPoolSize int
	RequestTrackingBufferSize     int
	RequestTrackingTimeoutSeconds int

	// Server
	ServerShutdownTimeoutSeconds int
}

var AppConfig *Config

func LoadConfig() {
	// Load .env file if it exists
	if err := godotenv.Load(); err != nil {
		log.Println("No .env file found, using environment variables")
	}

	AppConfig = &Config{
		Port:    getEnvOrDefault("PORT", "8080"),
		GinMode: getEnvOrDefault("GIN_MODE", "debug"),

		// Firebase
		FirebaseProjectID: getEnvOrDefault("FIREBASE_PROJECT_ID", "enchanted-login-8fdb9"),

		// Database
		DatabaseURL: getEnvOrDefault("DATABASE_URL", "postgres://localhost/tee_api?sslmode=disable"),

		// Google
		GoogleClientID:     getEnvOrDefault("GOOGLE_CLIENT_ID", ""),
		GoogleClientSecret: getEnvOrDefault("GOOGLE_CLIENT_SECRET", ""),

		// Slack
		SlackClientID:     getEnvOrDefault("SLACK_CLIENT_ID", ""),
		SlackClientSecret: getEnvOrDefault("SLACK_CLIENT_SECRET", ""),

		// Twitter
		TwitterClientID: getEnvOrDefault("TWITTER_CLIENT_ID", ""),

		// Composio
		ComposioAPIKey:        getEnvOrDefault("COMPOSIO_API_KEY", ""),
		ComposioTwitterConfig: getEnvOrDefault("COMPOSIO_TWITTER_CONFIG", ""),

		// OpenAI
		OpenAIAPIKey:     getEnvOrDefault("OPENAI_API_KEY", ""),
		OpenRouterAPIKey: getEnvOrDefault("OPENROUTER_API_KEY", ""),

		// Tinfoil
		TinfoilAPIKey: getEnvOrDefault("TINFOIL_API_KEY", ""),

		// Validator
		ValidatorType:    getEnvOrDefault("VALIDATOR_TYPE", "firebase"),
		JWTJWKSURL:       getEnvOrDefault("JWT_JWKS_URL", ""),
		FirebaseCredJSON: getEnvOrDefault("FIREBASE_CRED_JSON", ""),

		// MCP
		PerplexityAPIKey:  getEnvOrDefault("PERPLEXITY_API_KEY", ""),
		ReplicateAPIToken: getEnvOrDefault("REPLICATE_API_TOKEN", ""),

		// Rate Limiting
		RateLimitEnabled:        getEnvOrDefault("RATE_LIMIT_ENABLED", "true") == "true",
		RateLimitRequestsPerDay: getEnvAsInt64("RATE_LIMIT_REQUESTS_PER_DAY", 100),
		RateLimitLogOnly:        getEnvOrDefault("RATE_LIMIT_LOG_ONLY", "true") == "true",

		// Telegram
		TelegramToken: getEnvOrDefault("TELEGRAM_TOKEN", ""),
		NatsURL:       getEnvOrDefault("NATS_URL", ""),
		// Database Connection Pool
		DBMaxOpenConns:    getEnvAsInt("DB_MAX_OPEN_CONNS", 15),
		DBMaxIdleConns:    getEnvAsInt("DB_MAX_IDLE_CONNS", 5),
		DBConnMaxIdleTime: getEnvAsInt("DB_CONN_MAX_IDLE_TIME_MINUTES", 1),
		DBConnMaxLifetime: getEnvAsInt("DB_CONN_MAX_LIFETIME_MINUTES", 30),

		// Worker Pool
		RequestTrackingWorkerPoolSize: getEnvAsInt("REQUEST_TRACKING_WORKER_POOL_SIZE", 10),
		RequestTrackingBufferSize:     getEnvAsInt("REQUEST_TRACKING_BUFFER_SIZE", 1000),
		RequestTrackingTimeoutSeconds: getEnvAsInt("REQUEST_TRACKING_TIMEOUT_SECONDS", 30),

		// Server
		ServerShutdownTimeoutSeconds: getEnvAsInt("SERVER_SHUTDOWN_TIMEOUT_SECONDS", 30),
	}

	// Validate required configs
	if AppConfig.GoogleClientID == "" || AppConfig.SlackClientID == "" || AppConfig.TwitterClientID == "" {
		log.Println("Warning: Some OAuth client IDs are missing. Please check your environment variables.")
	}

	if AppConfig.FirebaseProjectID == "" {
		log.Println("Warning: Firebase project ID is missing. Please set FIREBASE_PROJECT_ID environment variable.")
	}

	if AppConfig.ComposioAPIKey == "" {
		log.Println("Warning: Composio API key is missing. Please set COMPOSIO_API_KEY environment variable.")
	}

	if AppConfig.PerplexityAPIKey == "" {
		log.Println("Warning: Perplexity API key is missing. Please set PERPLEXITY_API_KEY environment variable.")
	}

	if AppConfig.ReplicateAPIToken == "" {
		log.Println("Warning: Replicate API token is missing. Please set REPLICATE_API_TOKEN environment variable.")
	}

	if AppConfig.TelegramToken != "" {
		log.Println("Telegram service enabled with token")
	}

	log.Println("Firebase project ID: ", AppConfig.FirebaseProjectID)
}

func getEnvOrDefault(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

func getEnvAsInt64(key string, defaultValue int64) int64 {
	if value := os.Getenv(key); value != "" {
		if parsed, err := strconv.ParseInt(value, 10, 64); err == nil {
			return parsed
		} else {
			log.Printf("Warning: Failed to parse environment variable %s='%s' as int64, using default %d: %v", key, value, defaultValue, err)
		}
	}
	return defaultValue
}

func getEnvAsInt(key string, defaultValue int) int {
	if value := os.Getenv(key); value != "" {
		if parsed, err := strconv.Atoi(value); err == nil {
			return parsed
		} else {
			log.Printf("Warning: Failed to parse environment variable %s='%s' as int, using default %d: %v", key, value, defaultValue, err)
		}
	}
	return defaultValue
}
