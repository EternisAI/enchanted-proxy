package logger

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"os"
	"time"

	"github.com/lmittmann/tint"
)

// instanceID is a unique identifier for this server instance.
// Used to correlate logs across distributed deployments.
var instanceID string

func init() {
	// Try environment variables first (Kubernetes sets HOSTNAME)
	instanceID = os.Getenv("INSTANCE_ID")
	if instanceID == "" {
		instanceID = os.Getenv("HOSTNAME")
	}
	if instanceID == "" {
		instanceID = os.Getenv("POD_NAME")
	}
	// Generate random ID as fallback
	if instanceID == "" {
		b := make([]byte, 4)
		rand.Read(b)
		instanceID = hex.EncodeToString(b)
	}
}

// GetInstanceID returns the instance ID for this server.
func GetInstanceID() string {
	return instanceID
}

// Config holds the configuration of the logger.
type Config struct {
	Level  slog.Level
	Format string
}

// contextKey is used for context values.
type contextKey string

const (
	// ContextKeyRequestID is the key for request ID in the context.
	ContextKeyRequestID contextKey = "request_id"
	// ContextKeyUserID is the key for user ID in the context.
	ContextKeyUserID contextKey = "user_id"
	// ContextKeyChatID is the key for chat ID in the context.
	ContextKeyChatID contextKey = "chat_id"
	// ContextKeyOperation is the key for operation name in the context.
	ContextKeyOperation contextKey = "operation"
)

// Logger wraps slog.Logger.
type Logger struct {
	*slog.Logger
}

// New creates a new logger with the given config.
func New(config Config) *Logger {
	if config.Format == "json" {
		opts := &slog.HandlerOptions{
			Level:     config.Level,
			AddSource: true,
			ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
				// Better timestamp format.
				if a.Key == slog.TimeKey {
					return slog.Attr{
						Key:   a.Key,
						Value: slog.StringValue(a.Value.Time().Format(time.RFC3339)),
					}
				}
				return a
			},
		}
		// Add instance_id to all logs for distributed tracing
		return &Logger{
			Logger: slog.New(slog.NewJSONHandler(os.Stdout, opts)).With(slog.String("instance_id", instanceID)),
		}
	}

	opts := &tint.Options{
		Level:      config.Level,
		AddSource:  true,
		TimeFormat: time.Kitchen,
	}

	// Add instance_id to all logs for distributed tracing
	return &Logger{
		Logger: slog.New(tint.NewHandler(os.Stdout, opts)).With(slog.String("instance_id", instanceID)),
	}
}

// FromConfig creates a logger configuration from the main config.
func FromConfig(logLevel, logFormat string) Config {
	config := Config{
		Level:  slog.LevelDebug,
		Format: "text",
	}

	switch logLevel {
	case "debug":
		config.Level = slog.LevelDebug
	case "info":
		config.Level = slog.LevelInfo
	case "warn":
		config.Level = slog.LevelWarn
	case "error":
		config.Level = slog.LevelError
	}

	if logFormat != "" {
		config.Format = logFormat
	}

	// Use JSON format in production.
	if env := os.Getenv("APP_ENV"); env == "production" {
		config.Format = "json"
	}

	return config
}

// WithContext creates a new logger with context-specific attributes.
func (l *Logger) WithContext(ctx context.Context) *Logger {
	logger := l.Logger

	if requestID, ok := ctx.Value(ContextKeyRequestID).(string); ok && requestID != "" {
		logger = logger.With(slog.String("request_id", requestID))
	}

	if userID, ok := ctx.Value(ContextKeyUserID).(string); ok && userID != "" {
		logger = logger.With(slog.String("user_id", userID))
	}

	if chatID, ok := ctx.Value(ContextKeyChatID).(string); ok && chatID != "" {
		logger = logger.With(slog.String("chat_id", chatID))
	}

	if operation, ok := ctx.Value(ContextKeyOperation).(string); ok && operation != "" {
		logger = logger.With(slog.String("operation", operation))
	}

	return &Logger{
		Logger: logger,
	}
}

// WithComponent creates a new logger with a component name.
func (l *Logger) WithComponent(component string) *Logger {
	return &Logger{
		Logger: l.With(slog.String("component", component)),
	}
}

// WithFields creates a new logger with additional fields.
func (l *Logger) WithFields(fields map[string]interface{}) *Logger {
	args := make([]interface{}, 0, len(fields)*2)
	for k, v := range fields {
		args = append(args, k, v)
	}
	return &Logger{
		Logger: l.With(args...),
	}
}

// LogError logs an error with additional context.
func (l *Logger) LogError(ctx context.Context, err error, msg string, args ...interface{}) {
	logger := l.WithContext(ctx)
	allArgs := append([]interface{}{"error", err}, args...)
	logger.Error(msg, allArgs...)
}

// LogOperation logs the start and end of an operation.
// Useful for timing operations.
func (l *Logger) LogOperation(ctx context.Context, operation string, fn func() error) error {
	start := time.Now()
	logger := l.WithContext(ctx).With(slog.String("operation", operation))

	logger.Info("operation started")

	err := fn()
	duration := time.Since(start)

	if err != nil {
		logger.Error("operation failed",
			slog.Duration("duration", duration),
			slog.String("error", err.Error()),
		)
	} else {
		logger.Info("operation completed",
			slog.Duration("duration", duration),
		)
	}

	return err
}
