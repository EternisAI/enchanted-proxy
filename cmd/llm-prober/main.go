package main

import (
	"context"
	"flag"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/eternisai/enchanted-proxy/internal/config"
	"github.com/eternisai/enchanted-proxy/internal/logger"
	"github.com/eternisai/enchanted-proxy/internal/probe"
	"github.com/eternisai/enchanted-proxy/internal/routing"
)

func main() {
	configFile := flag.String("config", "", "path to config YAML (default: CONFIG_FILE env or config/config.yaml)")
	listenAddr := flag.String("listen", ":9090", "address for metrics server")
	logLevel := flag.String("log-level", "info", "log level (debug, info, warn, error)")
	logFormat := flag.String("log-format", "", "log format (json or text)")
	flag.Parse()

	// Resolve config file path: flag > env > default.
	cfgPath := *configFile
	if cfgPath == "" {
		cfgPath = os.Getenv("CONFIG_FILE")
		if cfgPath == "" {
			cfgPath = "config/config.yaml"
		}
	}

	// Initialize logger.
	logCfg := logger.FromConfig(*logLevel, *logFormat)
	appLogger := logger.New(logCfg)
	appLog := appLogger.WithComponent("main")

	appLog.Info("llm-prober starting",
		slog.String("config", cfgPath),
		slog.String("listen", *listenAddr))

	// Load config YAML. API keys are resolved from environment variables during
	// YAML unmarshaling (ModelProviderConfig.Validate reads APIKeyEnvVar).
	cfg := &config.Config{
		OpenRouterMobileAPIKey:  os.Getenv("OPENROUTER_MOBILE_API_KEY"),
		OpenRouterDesktopAPIKey: os.Getenv("OPENROUTER_DESKTOP_API_KEY"),
	}

	f, err := os.Open(cfgPath)
	if err != nil {
		log.Fatalf("failed to open config file: %v", err)
	}
	defer f.Close()

	if err := config.LoadConfigFile(f, cfg); err != nil {
		log.Fatalf("failed to load config file: %v", err)
	}

	if cfg.ModelRouterConfig == nil {
		log.Fatal("model router configuration is empty")
	}

	// Build model router (only for route/endpoint data, not request routing).
	router := routing.NewModelRouter(cfg, appLogger.WithComponent("routing"))
	if router == nil {
		log.Fatal("model router has no routes")
	}

	// Start probe service with endpoint deduplication.
	probeService := probe.NewProbeService(
		appLogger.WithComponent("probe"),
		router,
		cfg.ModelRouterConfig.Models,
	)

	// Start metrics HTTP server.
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	mux.HandleFunc("/healthz/live", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	server := &http.Server{
		Addr:              *listenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	go func() {
		appLog.Info("metrics server listening", slog.String("addr", *listenAddr))
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("metrics server error: %v", err)
		}
	}()

	// Wait for shutdown signal.
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	sig := <-quit
	appLog.Info("received signal, shutting down", slog.String("signal", sig.String()))

	probeService.Shutdown()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := server.Shutdown(ctx); err != nil {
		appLog.Error("metrics server shutdown error", slog.String("error", err.Error()))
	}

	appLog.Info("llm-prober stopped")
}
