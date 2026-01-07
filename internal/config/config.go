package config

import (
	"crypto/sha256"
	"io"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/goccy/go-yaml"
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

	// Model Router
	ModelRouterConfig *ModelRouterConfig `yaml:"model_router"`

	// Model Router Fallback Service
	FallbackPrometheusURL   string
	FallbackPrometheusToken string
	FallbackMinInterval     time.Duration

	// MCP
	PerplexityAPIKey  string
	ReplicateAPIToken string

	// Rate Limiting
	RateLimitEnabled    bool
	RateLimitLogOnly    bool // If true, only log violations, don't block.
	RateLimitFailClosed bool // If true, fail closed when tier config unavailable (503 error).

	// Deep Research Rate Limiting
	DeepResearchRateLimitEnabled bool // If false, skip freemium quota checks

	// Usage Tiers - Plan Token Quotas
	FreeMonthlyPlanTokens int64 // Free tier: 20k plan tokens/month
	ProDailyPlanTokens    int64 // Pro tier: 500k plan tokens/day

	// App Store (IAP)
	AppStoreAPIKeyP8 string
	AppStoreAPIKeyID string
	AppStoreBundleID string
	AppStoreIssuerID string

	// Stripe Configuration
	StripeSecretKey     string
	StripeWebhookSecret string
	StripeWeeklyPriceID string // Weekly subscription price ID (eligible for 3-day free trial)

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
	MessageStorageEnabled           bool // Enable/disable encrypted message storage to Firestore
	MessageStorageRequireEncryption bool // If true, refuse to store messages when encryption fails (strict E2EE mode). If false, fallback to plaintext storage (default: graceful degradation)
	MessageStorageWorkerPoolSize    int  // Number of worker goroutines processing message queue (higher = more concurrent Firestore writes)
	MessageStorageBufferSize        int  // Size of message queue channel (higher = handles bigger traffic spikes without dropping messages)
	MessageStorageTimeoutSeconds    int  // Firestore operation timeout in seconds (prevents workers from hanging on slow/failed operations)

	// Background Polling (for GPT-5 Pro and other long-running models)
	BackgroundPollingEnabled     bool // Enable background polling mode for GPT-5 Pro (recommended to avoid timeouts)
	BackgroundPollingInterval    int  // Seconds between OpenAI status polls (default: 2, increases to max after initial phase)
	BackgroundPollingMaxInterval int  // Maximum seconds between polls (default: 10, used after initial rapid polling)
	BackgroundPollingTimeout     int  // Minutes before giving up on polling (default: 30)
	BackgroundMaxConcurrentPolls int  // Maximum number of concurrent polling workers (default: 100)

	// Push Notifications
	PushNotificationsEnabled bool // Enable/disable FCM push notifications for task completions (default: true)

	// ZCash Backend
	ZCashBackendURL           string  // URL of zcash-payment-backend (default: http://127.0.0.1:20002)
	ZCashBackendAPIKey        string
	ZCashBackendSkipTLSVerify bool    // Skip TLS verification (for local dev only)
	ZCashDebugMultiplier      float64 // Price multiplier for testing (e.g., 0.01 for 1% of normal price, 0 = disabled)

	// Linear API (problem reports)
	LinearAPIKey    string
	LinearTeamID    string
	LinearProjectID string
	LinearLabelID   string
}

var (
	AppConfig *Config

	DefaultFallbackCheckInterval = 15 * time.Second
)

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

		// Model Router Fallback Service
		FallbackPrometheusURL:   getEnvOrDefault("FALLBACK_PROMETHEUS_URL", ""),
		FallbackPrometheusToken: getEnvOrDefault("FALLBACK_PROMETHEUS_TOKEN", ""),
		FallbackMinInterval:     getEnvAsDuration("FALLBACK_CHECK_INTERVAL", DefaultFallbackCheckInterval),

		// MCP
		PerplexityAPIKey:  getEnvOrDefault("PERPLEXITY_API_KEY", ""),
		ReplicateAPIToken: getEnvOrDefault("REPLICATE_API_TOKEN", ""),

		// Rate Limiting
		RateLimitEnabled:    getEnvOrDefault("RATE_LIMIT_ENABLED", "true") == "true",
		RateLimitLogOnly:    getEnvOrDefault("RATE_LIMIT_LOG_ONLY", "true") == "true",
		RateLimitFailClosed: getEnvOrDefault("RATE_LIMIT_FAIL_CLOSED", "false") == "true",

		// Deep Research Rate Limiting
		DeepResearchRateLimitEnabled: getEnvOrDefault("DEEP_RESEARCH_RATE_LIMIT_ENABLED", "true") == "true",

		// Usage Tiers - Plan Token Quotas
		FreeMonthlyPlanTokens: getEnvAsInt64("FREE_MONTHLY_PLAN_TOKENS", 20000),
		ProDailyPlanTokens:    getEnvAsInt64("PRO_DAILY_PLAN_TOKENS", 500000),

		// App Store (IAP)
		AppStoreAPIKeyP8: getEnvOrDefault("APPSTORE_API_KEY_P8", ""),
		AppStoreAPIKeyID: getEnvOrDefault("APPSTORE_API_KEY_ID", ""),
		AppStoreBundleID: getEnvOrDefault("APPSTORE_BUNDLE_ID", ""),
		AppStoreIssuerID: getEnvOrDefault("APPSTORE_ISSUER_ID", ""),

		// Stripe (trim whitespace to avoid common config errors)
		StripeSecretKey:     strings.TrimSpace(getEnvOrDefault("STRIPE_SECRET_KEY", "")),
		StripeWebhookSecret: strings.TrimSpace(getEnvOrDefault("STRIPE_WEBHOOK_SECRET", "")),
		StripeWeeklyPriceID: strings.TrimSpace(getEnvOrDefault("STRIPE_WEEKLY_PRICE_ID", "")),

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
		RequestTrackingWorkerPoolSize: getEnvAsInt("REQUEST_TRACKING_WORKER_POOL_SIZE", 20),
		RequestTrackingBufferSize:     getEnvAsInt("REQUEST_TRACKING_BUFFER_SIZE", 5000),
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

		// Background Polling
		BackgroundPollingEnabled:     getEnvOrDefault("BACKGROUND_POLLING_ENABLED", "true") == "true",
		BackgroundPollingInterval:    getEnvAsInt("BACKGROUND_POLLING_INTERVAL", 2),
		BackgroundPollingMaxInterval: getEnvAsInt("BACKGROUND_POLLING_MAX_INTERVAL", 10),
		BackgroundPollingTimeout:     getEnvAsInt("BACKGROUND_POLLING_TIMEOUT", 30),
		BackgroundMaxConcurrentPolls: getEnvAsInt("BACKGROUND_MAX_CONCURRENT_POLLS", 100),

		// Push Notifications
		PushNotificationsEnabled: getEnvOrDefault("PUSH_NOTIFICATIONS_ENABLED", "true") == "true",

		// ZCash Backend
		ZCashBackendURL:           getEnvOrDefault("ZCASH_BACKEND_URL", "http://127.0.0.1:20002"),
		ZCashBackendAPIKey:        getEnvOrDefault("ZCASH_BACKEND_API_KEY", ""),
		ZCashBackendSkipTLSVerify: getEnvOrDefault("ZCASH_BACKEND_SKIP_TLS_VERIFY", "false") == "true",
		ZCashDebugMultiplier:      getEnvFloat("ZCASH_DEBUG_MULTIPLIER", 0),

		// Linear API (problem reports)
		LinearAPIKey:    getEnvOrDefault("LINEAR_API_KEY", ""),
		LinearLabelID:   getEnvOrDefault("LINEAR_LABEL_ID", ""),
		LinearProjectID: getEnvOrDefault("LINEAR_PROJECT_ID", ""),
		LinearTeamID:    getEnvOrDefault("LINEAR_TEAM_ID", ""),
	}

	// Load settings from a configuration file.
	//
	// TODO: environment variables should have higher precedence, but this would require
	// a significant rework. For now, only use the config file for settings that should
	// not be overridden by environment variables, like model router configuration.
	// Later should replace this with proper config handling using spf13/viper.
	configFilePath := getEnvOrDefault("CONFIG_FILE", "config.yaml")
	log.Printf("Loading config file: %v", configFilePath)

	configFile, err := os.Open(configFilePath)
	defer func() {
		if configFile != nil {
			configFile.Close()
		}
	}()

	if err != nil {
		log.Fatalf("Failed to open config file: %v", err)
	}

	if err := LoadConfigFile(configFile, AppConfig); err != nil {
		log.Fatalf("Failed to load config file: %v", err)
	}

	// Validate required configs
	if AppConfig.ModelRouterConfig == nil {
		log.Fatal("Model Router configuration is empty")
	}

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

	if AppConfig.ZCashBackendAPIKey == "" {
		log.Println("Warning: ZCash Backend API key is missing. Please set ZCASH_BACKEND_API_KEY environment variable.")
	}

	if AppConfig.LinearAPIKey == "" {
		log.Println("Warning: Linear API key is missing. Please set LINEAR_API_KEY environment variable.")
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

	// Stripe configuration validation
	if AppConfig.StripeSecretKey == "" || AppConfig.StripeWebhookSecret == "" {
		log.Println("Warning: Stripe credentials are missing. Please set STRIPE_SECRET_KEY and STRIPE_WEBHOOK_SECRET environment variables.")
	} else {
		// Show first 12 chars of key for debugging (e.g., "sk_test_xxxx" or "sk_live_xxxx")
		keyPrefix := AppConfig.StripeSecretKey
		if len(keyPrefix) > 12 {
			keyPrefix = keyPrefix[:12] + "..."
		}
		log.Printf("Stripe configured: key=%s (length=%d), webhook_secret length=%d",
			keyPrefix, len(AppConfig.StripeSecretKey), len(AppConfig.StripeWebhookSecret))
	}

	log.Println("Firebase project ID: ", AppConfig.FirebaseProjectID)
}

func getEnvOrDefault(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

func getEnvAsDuration(key string, defaultValue time.Duration) time.Duration {
	if value := os.Getenv(key); value != "" {
		if parsed, err := time.ParseDuration(value); err == nil {
			return parsed
		} else {
			log.Printf("Warning: Failed to parse environment variable %s='%s' as time.Duration, using default %v: %v", key, value, defaultValue, err)
		}
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

func getEnvFloat(key string, defaultValue float64) float64 {
	if value := os.Getenv(key); value != "" {
		if parsed, err := strconv.ParseFloat(value, 64); err == nil {
			return parsed
		} else {
			log.Printf("Warning: Failed to parse environment variable %s='%s' as float, using default %f: %v", key, value, defaultValue, err)
		}
	}
	return defaultValue
}

func LoadConfigFile(reader io.Reader, config *Config) error {
	decoder := yaml.NewDecoder(reader)

	if err := decoder.Decode(config); err != nil {
		return err
	}

	return nil
}
