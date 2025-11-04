package config

import (
	"crypto/sha256"
	"log"
	"os"
	"strconv"

	"github.com/joho/godotenv"
)

type Config struct {
	Port                    string
	GinMode                 string
	FirebaseProjectID       string
	DatabaseURL             string
	GoogleClientID          string
	GoogleClientSecret      string
	SlackClientID           string
	SlackClientSecret       string
	TwitterClientID         string
	TwitterClientSecret     string
	ComposioAPIKey          string
	ComposioTwitterConfig   string
	OpenAIAPIKey            string
	OpenRouterMobileAPIKey  string
	OpenRouterDesktopAPIKey string
	TinfoilAPIKey           string
	NearAPIKey              string
	EternisInferenceAPIKey  string
	SerpAPIKey              string
	ExaAPIKey               string
	ValidatorType           string // "jwk" or "firebase"
	JWTJWKSURL              string
	FirebaseCredJSON        string

	// MCP
	PerplexityAPIKey  string
	ReplicateAPIToken string

	// Rate Limiting
	RateLimitEnabled bool
	RateLimitLogOnly bool // If true, only log violations, don't block.

	// Deep Research Rate Limiting
	DeepResearchRateLimitEnabled bool // If false, skip freemium quota checks

	// Usage Tiers
	FreeLifetimeTokens int64
	DripDailyMessages  int64
	ProDailyTokens     int64

	// App Store (IAP)
	AppStoreAPIKeyP8 string
	AppStoreAPIKeyID string
	AppStoreBundleID string
	AppStoreIssuerID string

	// Telegram
	EnableTelegramServer bool
	TelegramToken        string
	NatsURL              string

	// Database Connection Pool
	DBMaxOpenConns    int
	DBMaxIdleConns    int
	DBConnMaxIdleTime int // in minutes
	DBConnMaxLifetime int // in minutes

	// HTTP Transport Connection Pool
	ProxyMaxIdleConns        int
	ProxyMaxIdleConnsPerHost int
	ProxyMaxConnsPerHost     int
	ProxyIdleConnTimeout     int // in seconds

	// Worker Pool
	RequestTrackingWorkerPoolSize int
	RequestTrackingBufferSize     int
	RequestTrackingTimeoutSeconds int

	// Server
	ServerShutdownTimeoutSeconds int

	// CORS
	CORSAllowedOrigins string

	// Logging
	LogLevel  string
	LogFormat string

	// Temporal
	TemporalAPIKey    string
  TemporalEndpoint  string
	TemporalNamespace string
	// Message Storage
	MessageStorageEnabled         bool // Enable/disable encrypted message storage to Firestore
	MessageStorageRequireEncryption bool // If true, refuse to store messages when encryption fails (strict E2EE mode). If false, fallback to plaintext storage (default: graceful degradation)  
	MessageStorageWorkerPoolSize  int  // Number of worker goroutines processing message queue (higher = more concurrent Firestore writes)
	MessageStorageBufferSize      int  // Size of message queue channel (higher = handles bigger traffic spikes without dropping messages)
	MessageStorageTimeoutSeconds  int  // Firestore operation timeout in seconds (prevents workers from hanging on slow/failed operations)
}

var AppConfig *Config

func LoadConfig() {
	// Load .env file if it exists
	if err := godotenv.Load(".env"); err != nil {
		log.Println("No .env file found, using environment variables")
	}

	AppConfig = &Config{
		Port:    getEnvOrDefault("PORT", "8080"),
		GinMode: getEnvOrDefault("GIN_MODE", "release"),

		// Firebase
		FirebaseProjectID: getEnvOrDefault("FIREBASE_PROJECT_ID", "silo-dev-95230"),

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
		OpenAIAPIKey: getEnvOrDefault("OPENAI_API_KEY", ""),

		// OpenRouter
		OpenRouterMobileAPIKey:  getEnvOrDefault("OPENROUTER_MOBILE_API_KEY", ""),
		OpenRouterDesktopAPIKey: getEnvOrDefault("OPENROUTER_DESKTOP_API_KEY", ""),

		// Tinfoil
		TinfoilAPIKey: getEnvOrDefault("TINFOIL_API_KEY", ""),

		// Self-hosted inference APIs
		EternisInferenceAPIKey: getEnvOrDefault("ETERNIS_INFERENCE_API_KEY", ""),

		// Near
		NearAPIKey: getEnvOrDefault("NEAR_API_KEY", ""),

		// SerpAPI
		SerpAPIKey: getEnvOrDefault("SERPAPI_API_KEY", ""),

		// Exa AI
		ExaAPIKey: getEnvOrDefault("EXA_API_KEY", ""),

		// Validator
		ValidatorType:    getEnvOrDefault("VALIDATOR_TYPE", "firebase"),
		JWTJWKSURL:       getEnvOrDefault("JWT_JWKS_URL", ""),
		FirebaseCredJSON: getEnvOrDefault("FIREBASE_CRED_JSON", ""),

		// MCP
		PerplexityAPIKey:  getEnvOrDefault("PERPLEXITY_API_KEY", ""),
		ReplicateAPIToken: getEnvOrDefault("REPLICATE_API_TOKEN", ""),

		// Rate Limiting
		RateLimitEnabled: getEnvOrDefault("RATE_LIMIT_ENABLED", "true") == "true",
		RateLimitLogOnly: getEnvOrDefault("RATE_LIMIT_LOG_ONLY", "true") == "true",

		// Deep Research Rate Limiting
		DeepResearchRateLimitEnabled: getEnvOrDefault("DEEP_RESEARCH_RATE_LIMIT_ENABLED", "true") == "true",

		// Usage Tiers
		FreeLifetimeTokens: getEnvAsInt64("FREE_LIFETIME_TOKENS", 20000),
		DripDailyMessages:  getEnvAsInt64("DRIP_DAILY_MESSAGES", 10),
		ProDailyTokens:     getEnvAsInt64("PRO_DAILY_TOKENS", 500000),

		// App Store (IAP)
		AppStoreAPIKeyP8: getEnvOrDefault("APPSTORE_API_KEY_P8", ""),
		AppStoreAPIKeyID: getEnvOrDefault("APPSTORE_API_KEY_ID", ""),
		AppStoreBundleID: getEnvOrDefault("APPSTORE_BUNDLE_ID", ""),
		AppStoreIssuerID: getEnvOrDefault("APPSTORE_ISSUER_ID", ""),

		// Telegram
		EnableTelegramServer: getEnvOrDefault("ENABLE_TELEGRAM_SERVER", "true") == "true",
		TelegramToken:        getEnvOrDefault("TELEGRAM_TOKEN", ""),
		NatsURL:              getEnvOrDefault("NATS_URL", ""),

		// Database Connection Pool
		DBMaxOpenConns:    getEnvAsInt("DB_MAX_OPEN_CONNS", 15),
		DBMaxIdleConns:    getEnvAsInt("DB_MAX_IDLE_CONNS", 5),
		DBConnMaxIdleTime: getEnvAsInt("DB_CONN_MAX_IDLE_TIME_MINUTES", 1),
		DBConnMaxLifetime: getEnvAsInt("DB_CONN_MAX_LIFETIME_MINUTES", 30),

		// HTTP Transport Connection Pool
		ProxyMaxIdleConns:        getEnvAsInt("PROXY_MAX_IDLE_CONNS", 100),
		ProxyMaxIdleConnsPerHost: getEnvAsInt("PROXY_MAX_IDLE_CONNS_PER_HOST", 50),
		ProxyMaxConnsPerHost:     getEnvAsInt("PROXY_MAX_CONNS_PER_HOST", 100),
		ProxyIdleConnTimeout:     getEnvAsInt("PROXY_IDLE_CONN_TIMEOUT_SECONDS", 90),

		// Worker Pool
		RequestTrackingWorkerPoolSize: getEnvAsInt("REQUEST_TRACKING_WORKER_POOL_SIZE", 10),
		RequestTrackingBufferSize:     getEnvAsInt("REQUEST_TRACKING_BUFFER_SIZE", 1000),
		RequestTrackingTimeoutSeconds: getEnvAsInt("REQUEST_TRACKING_TIMEOUT_SECONDS", 30),

		// Server
		ServerShutdownTimeoutSeconds: getEnvAsInt("SERVER_SHUTDOWN_TIMEOUT_SECONDS", 30),

		// CORS
		CORSAllowedOrigins: getEnvOrDefault("CORS_ALLOWED_ORIGINS", "http://localhost:3000"),

		// Logging
		LogLevel:  getEnvOrDefault("LOG_LEVEL", "debug"),
		LogFormat: getEnvOrDefault("LOG_FORMAT", "text"),

		// Temporal
		TemporalAPIKey:    getEnvOrDefault("TEMPORAL_API_KEY", ""),
		TemporalEndpoint:  getEnvOrDefault("TEMPORAL_ENDPOINT", ""),
    TemporalNamespace: getEnvOrDefault("TEMPORAL_NAMESPACE", ""),
		// Message Storage
		MessageStorageEnabled:           getEnvOrDefault("MESSAGE_STORAGE_ENABLED", "true") == "true",
		MessageStorageRequireEncryption: getEnvOrDefault("MESSAGE_STORAGE_REQUIRE_ENCRYPTION", "false") == "true",
		MessageStorageWorkerPoolSize:    getEnvAsInt("MESSAGE_STORAGE_WORKER_POOL_SIZE", 5),
		MessageStorageBufferSize:        getEnvAsInt("MESSAGE_STORAGE_BUFFER_SIZE", 500),
		MessageStorageTimeoutSeconds:    getEnvAsInt("MESSAGE_STORAGE_TIMEOUT_SECONDS", 30),
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

	if AppConfig.SerpAPIKey == "" {
		log.Println("Warning: SerpAPI key is missing. Please set SERPAPI_API_KEY environment variable.")
	}

	if AppConfig.ExaAPIKey == "" {
		log.Println("Warning: Exa AI API key is missing. Please set EXA_API_KEY environment variable.")
	}

	if AppConfig.TelegramToken != "" {
		log.Println("Telegram service enabled with token")
	}

	if AppConfig.AppStoreAPIKeyP8 == "" || AppConfig.AppStoreAPIKeyID == "" || AppConfig.AppStoreBundleID == "" || AppConfig.AppStoreIssuerID == "" {
		log.Println("Warning: App Store IAP credentials are missing. Please set APPSTORE_API_KEY_P8, APPSTORE_API_KEY_ID, APPSTORE_BUNDLE_ID, and APPSTORE_ISSUER_ID environment variables.")
	} else {
		log.Println(
			"App Store IAP configured:",
			"key_id=", AppConfig.AppStoreAPIKeyID,
			"bundle_id=", AppConfig.AppStoreBundleID,
			"issuer_id=", AppConfig.AppStoreIssuerID,
		)

		if AppConfig.AppStoreAPIKeyP8 != "" {
			sum := sha256.Sum256([]byte(AppConfig.AppStoreAPIKeyP8))
			log.Printf("App Store IAP private key loaded (sha256=%x, bytes=%d)", sum, len(AppConfig.AppStoreAPIKeyP8))
		}
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
