package probe

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"sync"

	"github.com/eternisai/enchanted-proxy/internal/config"
	"github.com/eternisai/enchanted-proxy/internal/logger"
	"github.com/eternisai/enchanted-proxy/internal/routing"
)

// ProbeService manages active health probes for configured model endpoints.
// Each enabled endpoint gets its own goroutine that periodically sends a minimal
// chat completion request and records the result as Prometheus metrics.
type ProbeService struct {
	logger   *logger.Logger
	wg       sync.WaitGroup
	shutdown chan struct{}
	cancel   context.CancelFunc
}

// probeTarget holds deduplicated probe configuration, pairing the resolved
// endpoint data (for HTTP requests) with canonical names (for metric labels).
type probeTarget struct {
	provider       *routing.ProviderConfig
	probe          *routing.ProbeConfig
	providerName   string // for metrics labels
	canonicalModel string // for metrics labels (from config entry name)
}

// NewProbeService creates a new probe service and starts a probe worker goroutine
// for every unique (base_url, effective_model) combination. Models are iterated in
// config declaration order so the first canonical name encountered wins for metrics.
// Endpoints using the Responses API are skipped as they don't support standard
// chat completions.
func NewProbeService(logger *logger.Logger, router *routing.ModelRouter, models []config.ModelConfig) *ProbeService {
	ctx, cancel := context.WithCancel(context.Background())
	s := &ProbeService{
		logger:   logger,
		shutdown: make(chan struct{}),
		cancel:   cancel,
	}

	routes := router.GetRoutes()

	// Collect unique probe targets, iterating models in config declaration order
	// so the first canonical name encountered for each (base_url, effective_model) wins.
	seen := make(map[string]*probeTarget)
	var targets []*probeTarget

	for _, modelCfg := range models {
		route, exists := routes[modelCfg.Name]
		if !exists {
			continue
		}

		allEndpoints := make([]routing.ModelEndpoint, 0, len(route.ActiveEndpoints)+len(route.InactiveEndpoints))
		allEndpoints = append(allEndpoints, route.ActiveEndpoints...)
		allEndpoints = append(allEndpoints, route.InactiveEndpoints...)

		for _, endpoint := range allEndpoints {
			if endpoint.Probe == nil || !endpoint.Probe.Enabled {
				continue
			}

			// Skip Responses API endpoints — they don't support /chat/completions.
			if endpoint.Provider.APIType == config.APITypeResponses {
				logger.Info("skipping probe for responses API endpoint",
					slog.String("provider", endpoint.Provider.Name),
					slog.String("model", modelCfg.Name))
				continue
			}

			effectiveModel := endpoint.Provider.Model
			key := strings.TrimRight(endpoint.Provider.BaseURL, "/") + "|" + effectiveModel

			if existing, exists := seen[key]; exists {
				logger.Debug("skipping duplicate probe target",
					slog.String("canonical_model", modelCfg.Name),
					slog.String("effective_model", effectiveModel),
					slog.String("provider", endpoint.Provider.Name),
					slog.String("dedup_canonical", existing.canonicalModel))
				continue
			}

			target := &probeTarget{
				provider:       endpoint.Provider,
				probe:          endpoint.Probe,
				providerName:   endpoint.Provider.Name,
				canonicalModel: modelCfg.Name,
			}
			seen[key] = target
			targets = append(targets, target)
		}
	}

	// Create workers from deduplicated, ordered targets.
	for _, target := range targets {
		w := &probeWorker{
			service:  s,
			ctx:      ctx,
			provider: target.providerName,
			model:    target.canonicalModel,
			endpoint: target.provider,
			probe:    target.probe,
			client: &http.Client{
				Timeout: probeHTTPTimeout,
			},
			logger: logger,
		}

		s.wg.Add(1)
		go w.run()
	}

	logger.Info("probe service started",
		slog.Int("workers", len(targets)),
		slog.Int("deduplicated", len(seen)))

	return s
}

// Shutdown stops all probe workers and waits for them to finish.
func (s *ProbeService) Shutdown() {
	if s == nil {
		return
	}

	s.cancel()
	close(s.shutdown)
	s.wg.Wait()
	s.logger.Info("probe service stopped")
}
