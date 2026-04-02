package probe

import (
	"context"
	"log/slog"
	"net/http"
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

// NewProbeService creates a new probe service and starts a probe worker goroutine
// for every enabled model endpoint. Endpoints using the Responses API are skipped
// as they don't support standard chat completions.
func NewProbeService(logger *logger.Logger, router *routing.ModelRouter) *ProbeService {
	ctx, cancel := context.WithCancel(context.Background())
	s := &ProbeService{
		logger:   logger,
		shutdown: make(chan struct{}),
		cancel:   cancel,
	}

	routes := router.GetRoutes()
	workerCount := 0

	for model, route := range routes {
		// Skip wildcard route — no specific model to probe.
		if model == "*" {
			continue
		}

		// Probe all endpoints (active and inactive).
		allEndpoints := make([]routing.ModelEndpoint, 0, len(route.ActiveEndpoints)+len(route.InactiveEndpoints))
		allEndpoints = append(allEndpoints, route.ActiveEndpoints...)
		allEndpoints = append(allEndpoints, route.InactiveEndpoints...)

		for _, endpoint := range allEndpoints {
			// Skip endpoints without probe config (shouldn't happen with defaults, but be safe).
			if endpoint.Probe == nil || !endpoint.Probe.Enabled {
				continue
			}

			// Skip Responses API endpoints — they don't support /chat/completions.
			if endpoint.Provider.APIType == config.APITypeResponses {
				logger.Info("skipping probe for responses API endpoint",
					slog.String("provider", endpoint.Provider.Name),
					slog.String("model", model))
				continue
			}

			w := &probeWorker{
				service:  s,
				ctx:      ctx,
				provider: endpoint.Provider.Name,
				model:    model,
				endpoint: endpoint.Provider,
				probe:    endpoint.Probe,
				client: &http.Client{
					Timeout: probeHTTPTimeout,
				},
				logger: logger,
			}

			s.wg.Add(1)
			go w.run()
			workerCount++
		}
	}

	logger.Info("probe service started",
		slog.Int("workers", workerCount))

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
