package fallback

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/eternisai/enchanted-proxy/internal/config"
	"github.com/eternisai/enchanted-proxy/internal/logger"
	"github.com/eternisai/enchanted-proxy/internal/routing"
	promapi "github.com/prometheus/client_golang/api"
	promapiv1 "github.com/prometheus/client_golang/api/prometheus/v1"
	prommodel "github.com/prometheus/common/model"
)

type promRoundTripper struct {
	token        string
	roundTripper http.RoundTripper
}

func (rt *promRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.Header.Set("authorization", "Bearer "+rt.token)
	return rt.roundTripper.RoundTrip(req)
}

type FallbackService struct {
	api      promapiv1.API
	router   *routing.ModelRouter
	interval time.Duration

	ctx      context.Context
	logger   *logger.Logger
	mu       sync.Mutex
	wg       sync.WaitGroup
	shutdown chan struct{}
}

func NewFallbackService(appConfig *config.Config, logger *logger.Logger, router *routing.ModelRouter) *FallbackService {
	if appConfig.FallbackPrometheusURL == "" {
		logger.Warn("Fallback Prometheus URL not configured - not starting Fallback Service")
		return nil
	}

	// Prepare a Prometheus query API client, with bearer token authorization if applicable.
	promCfg := promapi.Config{
		Address: appConfig.FallbackPrometheusURL,
	}

	if appConfig.FallbackPrometheusToken != "" {
		promCfg.RoundTripper = &promRoundTripper{
			token:        appConfig.FallbackPrometheusToken,
			roundTripper: http.DefaultTransport,
		}
	}

	client, err := promapi.NewClient(promCfg)
	if err != nil {
		logger.Error("failed to initialize Prometheus API client", slog.String("error", err.Error()))
		return nil
	}

	s := &FallbackService{
		api:      promapiv1.NewAPI(client),
		router:   router,
		interval: appConfig.FallbackMinInterval,

		logger:   logger,
		shutdown: make(chan struct{}),
	}

	// Walk the initial routing table and launch a worker for every model endpoint that has
	// fallback policy configured.
	routes := router.GetRoutes()
	for model, route := range routes {
		for _, endpoint := range route.ActiveEndpoints {
			if endpoint.Fallback != nil {
				worker := &fallbackWorker{
					service:  s,
					model:    model,
					provider: endpoint.Provider.Name,
					config:   endpoint.Fallback,
				}

				s.wg.Add(1)
				go worker.run()
			}
		}
	}

	return s
}

func (s *FallbackService) Shutdown() {
	if s == nil {
		return
	}

	// Stop the worker pool.
	close(s.shutdown)
	s.wg.Wait()
}

type fallbackWorker struct {
	service *FallbackService

	model    string
	provider string
	config   *routing.FallbackConfig

	triggered bool
}

func (w *fallbackWorker) run() {
	defer w.service.wg.Done()

	w.service.logger.Info("started fallback worker",
		slog.String("model", w.model),
		slog.String("provider", w.provider))
	defer w.service.logger.Info("stopped fallback worker",
		slog.String("model", w.model),
		slog.String("provider", w.provider))

	nextRunTime := time.Now()

	for {
		select {
		case <-time.After(time.Until(nextRunTime)):
			nextRunTime = w.refreshEndpoints(w.service.api, time.Now())
			if nextRunTime.IsZero() {
				return
			}
		case <-w.service.shutdown:
			return
		}
	}
}

// promQueryAPI is an interface emulating a Prometheus Query API client.
// Enables tests to emulate specific results of PromQL queries.
type promQueryAPI interface {
	Query(ctx context.Context, query string, ts time.Time, opts ...promapiv1.Option) (prommodel.Value, promapiv1.Warnings, error)
}

// promQueryResult is a convenience struct to send the result of a Prometheus query over a channel.
type promQueryResult struct {
	value    prommodel.Value
	warnings promapiv1.Warnings
	err      error
}

// refreshEndpoints is the main executor of the fallback policy logic for a specific model endpoint.
//
// It runs on a specific interval: either a hysteresis interval specified by "dwell time" settings
// of a trigger/recover even (after that event has triggered) or the default periodic check interval
// configured in the app config via FALLBACK_CHECK_INTERVAL environment variable (defaults to 15s,
// which should be no less than the metric scraping interval).
//
// During the run, it retrieves the result of the appropriate query (recover event query if in the
// triggered state, trigger event query if in the normal/recovered state).
//
// If the result of the query indicates that the state change event has triggered, endpoints are
// refreshed for the model to which the endpoint that triggered the state change belongs:
//   - The event that triggered the state change is either deactivated (on "trigger") or activated
//     (on "recover')
//   - We attempt to find a fallback endpoint and do the reverse operation on it (activate on
//     "trigger", deactivate on "recover')
//
// The routing map with the updated endpoint set is stored in the model router.
// As workers for different model endpoints run concurrently, the refresh-and-store operation
// is protected by a mutex.
//
// An appropriate time of the next run is returned (default interval if there was no state change,
// hysteresis interval / dwell time period if there was a state change).
func (w *fallbackWorker) refreshEndpoints(api promQueryAPI, now time.Time) time.Time {
	// By default, if there is no state change, run with the default check interval.
	nextRunTime := now.Add(w.service.interval)

	// Determine the appropriate PromQL query depending on the current state.
	var query string
	if w.triggered {
		// We are in the triggered (fallback) state: check for recovery.
		query = w.config.Recover.Query
	} else {
		// We are in the normal state: check for the fallback trigger.
		query = w.config.Trigger.Query
	}

	// Execute the PromQL query asynchronously so it does not block the app shutdown.
	var res promQueryResult
	resChan := make(chan promQueryResult)

	ctx, cancel := context.WithTimeout(context.Background(), w.service.interval)
	defer cancel()

	go func() {
		result, warnings, err := api.Query(ctx, query, now)
		resChan <- promQueryResult{result, warnings, err}
	}()

	select {
	case res = <-resChan:
		if res.err != nil {
			w.service.logger.Error("failed to fetch metrics",
				slog.Bool("fallback", w.triggered),
				slog.String("model", w.model),
				slog.String("provider", w.provider),
				slog.String("error", res.err.Error()))

			return nextRunTime
		}

		if len(res.warnings) > 0 {
			w.service.logger.Warn("warnings when fetching metrics",
				slog.Bool("fallback", w.triggered),
				slog.String("model", w.model),
				slog.String("provider", w.provider),
				slog.String("warnings", strings.Join(res.warnings, "; ")))
		}
	case <-w.service.shutdown:
		return time.Time{}
	}

	val, ok := res.value.(prommodel.Vector)
	if !ok {
		w.service.logger.Error("incorrect query returning non-vector",
			slog.Bool("fallback", w.triggered),
			slog.String("model", w.model),
			slog.String("provider", w.provider))
		return time.Time{}
	}

	// Empty result or 0 in the result mean no state change.
	if len(val) == 0 || val[0].Value < 1 {
		return nextRunTime
	}

	// A fallback or recovery check triggered - change the state and apply hysteresis.
	if w.triggered {
		w.triggered = false

		dwellTime := w.config.Recover.DwellTime
		if dwellTime == 0 {
			dwellTime = w.service.interval
		}

		nextRunTime = now.Add(dwellTime)

		w.service.logger.Info("recovery event",
			slog.String("model", w.model),
			slog.String("provider", w.provider),
			slog.String("dwell_until", nextRunTime.Format(time.RFC3339)))
	} else {
		w.triggered = true

		dwellTime := w.config.Trigger.DwellTime
		if dwellTime == 0 {
			dwellTime = w.service.interval
		}

		nextRunTime = now.Add(dwellTime)

		w.service.logger.Info("fallback event",
			slog.String("model", w.model),
			slog.String("provider", w.provider),
			slog.String("dwell_until", nextRunTime.Format(time.RFC3339)))
	}

	// Lock the routing table and refresh the endpoints for the model according to the event.
	w.service.mu.Lock()
	defer w.service.mu.Unlock()

	routes := w.service.router.GetRoutes()
	route := routes[w.model]

	activeEndpoints := make([]routing.ModelEndpoint, 0, len(route.ActiveEndpoints)+1)
	inactiveEndpoints := make([]routing.ModelEndpoint, 0, len(route.InactiveEndpoints)+1)

	endpointFlipped := false
	for _, endpoint := range route.ActiveEndpoints {
		// Flip one faillback endpoint (an endpoint that doesn't have its own fallback
		// configuration) from active to inactive in case of a recovery event (w.triggered == false).
		if endpoint.Fallback == nil && !w.triggered && !endpointFlipped {
			inactiveEndpoints = append(inactiveEndpoints, endpoint)
			endpointFlipped = true
			w.service.logger.Info("deactivated fallback endpoint",
				slog.String("model", w.model),
				slog.String("provider", endpoint.Provider.Name))
			continue
		}

		// Flip the endpoint for our provider from active to inactive in case of a fallback
		// event (w.triggered == true).
		if endpoint.Fallback != nil && w.triggered && endpoint.Provider.Name == w.provider {
			inactiveEndpoints = append(inactiveEndpoints, endpoint)
			w.service.logger.Info("deactivated primary endpoint",
				slog.String("model", w.model),
				slog.String("provider", w.provider))
			continue
		}

		activeEndpoints = append(activeEndpoints, endpoint)
	}

	endpointFlipped = false
	for _, endpoint := range route.InactiveEndpoints {
		// Flip one fallback endpoint (an endpoint that doesn't have its own fallback
		// configuration) from inactive to active in case of a fallback event (w.triggered == true).
		if endpoint.Fallback == nil && w.triggered && !endpointFlipped {
			activeEndpoints = append(activeEndpoints, endpoint)
			endpointFlipped = true
			w.service.logger.Info("activated fallback endpoint",
				slog.String("model", w.model),
				slog.String("provider", endpoint.Provider.Name))
			continue
		}

		// Flip the endpoint for our provider from inactive to active in case of a recovery
		// event (w.triggered == false).
		if endpoint.Fallback != nil && !w.triggered && endpoint.Provider.Name == w.provider {
			activeEndpoints = append(activeEndpoints, endpoint)
			w.service.logger.Info("activated primary endpoint",
				slog.String("model", w.model),
				slog.String("provider", w.provider))
			continue
		}

		inactiveEndpoints = append(inactiveEndpoints, endpoint)
	}

	// Apply the new routing table with updated endpoints.
	route = routing.ModelRoute{
		ActiveEndpoints:   activeEndpoints,
		InactiveEndpoints: inactiveEndpoints,
		RoundRobinCounter: route.RoundRobinCounter,
	}

	newRoutes := make(map[string]routing.ModelRoute, len(routes))
	for key, value := range routes {
		newRoutes[key] = value
	}

	newRoutes[w.model] = route
	w.service.router.SetRoutes(newRoutes)

	return nextRunTime
}
