package main

import (
	"context"
	"flag"
	"log"
	"log/slog"
	"net"
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
	stateDB := flag.String("state-db", "", "path to the persistent probe state database (default: LLM_PROBER_STATE_DB env; empty disables persistence)")
	stateFlush := flag.Duration("state-flush-interval", 5*time.Minute, "how often pending probe timestamps are coalesced into a single write")
	dumpState := flag.Bool("dump-state", false, "print the contents of the probe state database as JSON and exit")
	debugAddr := flag.String("debug-listen", "127.0.0.1:9091", "loopback address serving /debug/state; must be a loopback host; empty disables it")
	flag.Parse()

	// Resolve config file path: flag > env > default.
	cfgPath := *configFile
	if cfgPath == "" {
		cfgPath = os.Getenv("CONFIG_FILE")
		if cfgPath == "" {
			cfgPath = "config/config.yaml"
		}
	}

	// Resolve state database path: flag > env > disabled.
	statePath := *stateDB
	if statePath == "" {
		statePath = os.Getenv("LLM_PROBER_STATE_DB")
	}

	// -dump-state is an operator tool: read the database and exit without
	// touching config, the network, or the running prober's state.
	if *dumpState {
		if err := probe.DumpState(statePath, os.Stdout); err != nil {
			log.Fatalf("failed to dump probe state: %v", err)
		}
		return
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
		probe.Options{
			SlackWebhookURL:    os.Getenv("LLM_PROBER_SLACK_WEBHOOK_URL"),
			StateDBPath:        statePath,
			StateFlushInterval: *stateFlush,
		},
	)

	// Start metrics HTTP server.
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	mux.HandleFunc("/healthz/live", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/healthz/ready", func(w http.ResponseWriter, r *http.Request) {
		if !probeService.Ready() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
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

	// Start the debug server.
	//
	// This is deliberately a second listener rather than another route on the
	// metrics mux. The metrics port is bound on all interfaces so Prometheus can
	// scrape it, and it carries no authentication, so a route added there is
	// readable by anything that can reach the pod. The state view is only ever
	// consumed by an operator already inside the container, so it is bound to
	// loopback and reachable by nothing else.
	// The address is not taken on trust. /debug/state is unauthenticated, so a
	// value like ":9091" or "0.0.0.0:9091" would undo the isolation the separate
	// listener exists to provide. A non-loopback address disables the server
	// rather than failing startup, matching how the rest of this degrades.
	debugListen := *debugAddr
	if debugListen != "" && !isLoopbackAddr(debugListen) {
		appLog.Warn("refusing to serve /debug/state on a non-loopback address; state introspection disabled",
			slog.String("addr", debugListen))
		debugListen = ""
	}

	var debugServer *http.Server
	if debugListen != "" {
		debugMux := http.NewServeMux()
		// Reads through the already-open database: bbolt holds its file lock for
		// the lifetime of this process, so nothing outside it can open the
		// database while the prober runs.
		debugMux.HandleFunc("/debug/state", func(w http.ResponseWriter, r *http.Request) {
			records, err := probeService.StateSnapshot()
			if err != nil {
				http.Error(w, err.Error(), http.StatusServiceUnavailable)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			if err := probe.WriteDump(w, records); err != nil {
				appLog.Warn("failed to write state snapshot", slog.String("error", err.Error()))
			}
		})

		debugServer = &http.Server{
			Addr:              debugListen,
			Handler:           debugMux,
			ReadHeaderTimeout: 10 * time.Second,
			IdleTimeout:       60 * time.Second,
		}

		go func() {
			appLog.Info("debug server listening", slog.String("addr", debugListen))
			if err := debugServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				// Not fatal: losing state introspection must not stop probing.
				appLog.Warn("debug server error", slog.String("error", err.Error()))
			}
		}()
	}

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
	if debugServer != nil {
		if err := debugServer.Shutdown(ctx); err != nil {
			appLog.Error("debug server shutdown error", slog.String("error", err.Error()))
		}
	}

	appLog.Info("llm-prober stopped")
}

// isLoopbackAddr reports whether addr binds the loopback interface only.
//
// A missing host (":9091") binds every interface, so it is rejected along with
// any routable address. Only IP literals are accepted, plus "localhost", so the
// decision never depends on name resolution.
func isLoopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil || host == "" {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
