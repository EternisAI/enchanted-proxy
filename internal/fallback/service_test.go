package fallback

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/eternisai/enchanted-proxy/internal/config"
	"github.com/eternisai/enchanted-proxy/internal/logger"
	"github.com/eternisai/enchanted-proxy/internal/routing"
	promapiv1 "github.com/prometheus/client_golang/api/prometheus/v1"
	prommodel "github.com/prometheus/common/model"
)

const (
	model = "zai-org/GLM-4.6"
)

var (
	log *logger.Logger
)

type promQueryAPIEmulator struct {
	t     *testing.T
	query string
	value *float64
}

func newPromQueryAPIEmulator(t *testing.T, query string, values ...float64) promQueryAPI {
	emu := &promQueryAPIEmulator{
		t:     t,
		query: query,
	}

	if len(values) > 0 {
		emu.value = &values[0]
	}

	return emu
}

func (e *promQueryAPIEmulator) Query(
	ctx context.Context,
	query string,
	ts time.Time,
	opts ...promapiv1.Option,
) (prommodel.Value, promapiv1.Warnings, error) {
	if e.query != query {
		e.t.Errorf("Expected PromQL query %q, got %q", e.query, query)
	}

	var value prommodel.Value

	if e.value == nil {
		value = prommodel.Vector([]*prommodel.Sample{})
	} else {
		value = prommodel.Vector([]*prommodel.Sample{
			&prommodel.Sample{
				Metric:    prommodel.Metric(prommodel.LabelSet(map[prommodel.LabelName]prommodel.LabelValue{})),
				Value:     prommodel.SampleValue(*e.value),
				Timestamp: prommodel.Time(ts.Unix()),
			},
		})
	}

	return value, nil, nil
}

func newModelRouter(t *testing.T) *routing.ModelRouter {
	env := map[string]string{
		"ETERNIS_INFERENCE_API_KEY": "test-eternis-key",
		"NEAR_API_KEY":              "test-near-ai-key",
	}

	for key, value := range env {
		t.Setenv(key, value)
	}

	configFile, err := os.Open("testdata/config.yaml")
	defer func() {
		if configFile != nil {
			configFile.Close()
		}
	}()

	if err != nil {
		t.Fatalf("Failed to open config file: %v", err)
	}

	appConfig := new(config.Config)
	if err := config.LoadConfigFile(configFile, appConfig); err != nil {
		t.Fatalf("Failed to load config file: %v", err)
	}

	return routing.NewModelRouter(appConfig, log)
}

func TestMain(m *testing.M) {
	flag.Parse()

	if testing.Verbose() {
		log = logger.New(logger.Config{Level: slog.LevelDebug})
	} else {
		log = logger.New(logger.Config{Level: slog.LevelError})
	}

	exitCode := m.Run()

	os.Exit(exitCode)
}

func TestMaintainRecoveryState(t *testing.T) {
	router := newModelRouter(t)

	s := &FallbackService{
		router:   router,
		interval: config.DefaultFallbackCheckInterval,
		logger:   log,
	}

	routes := router.GetRoutes()
	route := routes[model]
	endpoint := route.ActiveEndpoints[0]

	w := &fallbackWorker{
		service:   s,
		model:     model,
		provider:  endpoint.Provider.Name,
		config:    endpoint.Fallback,
		triggered: false,
	}

	expectedProviderName := "Eternis"
	provider, err := router.RouteModel(model, "")
	if err != nil {
		t.Fatalf("RouteModel failed: %v", err)
	}

	if provider.Name != expectedProviderName {
		t.Errorf("Expected initial provider %s, got %s", expectedProviderName, provider.Name)
	}

	tests := map[string][]float64{
		"empty_response": []float64{},
		"zero_response":  []float64{0},
	}

	for name, values := range tests {
		t.Run(name, func(t *testing.T) {
			api := newPromQueryAPIEmulator(t, endpoint.Fallback.Trigger.Query, values...)
			now := time.Now()
			expectedNextRunTime := now.Add(s.interval)
			nextRunTime := w.refreshEndpoints(api, now)

			if !nextRunTime.Equal(expectedNextRunTime) {
				t.Errorf("Expected next run time %v, got %v", expectedNextRunTime, nextRunTime)
			}

			routes = router.GetRoutes()
			newRoute := routes[model]
			if len(newRoute.ActiveEndpoints) != len(route.ActiveEndpoints) {
				t.Errorf("Expected %d active endpoints, got %d", len(route.ActiveEndpoints), len(newRoute.ActiveEndpoints))
			}

			provider, err := router.RouteModel(model, "")
			if err != nil {
				t.Fatalf("RouteModel failed: %v", err)
			}

			if provider.Name != expectedProviderName {
				t.Errorf("Expected provider %s, got %s", expectedProviderName, provider.Name)
			}
		})
	}
}

func TestMaintainFallbackState(t *testing.T) {
	router := newModelRouter(t)

	s := &FallbackService{
		router:   router,
		interval: config.DefaultFallbackCheckInterval,
		logger:   log,
	}

	routes := router.GetRoutes()
	route := routes[model]
	endpoint := route.ActiveEndpoints[0]

	w := &fallbackWorker{
		service:   s,
		model:     model,
		provider:  endpoint.Provider.Name,
		config:    endpoint.Fallback,
		triggered: true,
	}

	route = routing.ModelRoute{
		ActiveEndpoints:   route.InactiveEndpoints,
		InactiveEndpoints: route.ActiveEndpoints,
		RoundRobinCounter: route.RoundRobinCounter,
	}
	routes[model] = route
	router.SetRoutes(routes)

	expectedProviderName := "NEAR AI"
	provider, err := router.RouteModel(model, "")
	if err != nil {
		t.Fatalf("RouteModel failed: %v", err)
	}

	if provider.Name != expectedProviderName {
		t.Errorf("Expected initial provider %s, got %s", expectedProviderName, provider.Name)
	}

	tests := map[string][]float64{
		"empty_response": []float64{},
		"zero_response":  []float64{0},
	}

	for name, values := range tests {
		t.Run(name, func(t *testing.T) {
			api := newPromQueryAPIEmulator(t, endpoint.Fallback.Recover.Query, values...)
			now := time.Now()
			expectedNextRunTime := now.Add(s.interval)
			nextRunTime := w.refreshEndpoints(api, now)

			if !nextRunTime.Equal(expectedNextRunTime) {
				t.Errorf("Expected next run time %v, got %v", expectedNextRunTime, nextRunTime)
			}

			routes = router.GetRoutes()
			newRoute := routes[model]
			if len(newRoute.ActiveEndpoints) != len(route.ActiveEndpoints) {
				t.Errorf("Expected %d active endpoints, got %d", len(route.ActiveEndpoints), len(newRoute.ActiveEndpoints))
			}

			provider, err := router.RouteModel(model, "")
			if err != nil {
				t.Fatalf("RouteModel failed: %v", err)
			}

			if provider.Name != expectedProviderName {
				t.Errorf("Expected provider %s, got %s", expectedProviderName, provider.Name)
			}
		})
	}
}

func TestFallbackTrigger(t *testing.T) {
	router := newModelRouter(t)

	s := &FallbackService{
		router:   router,
		interval: config.DefaultFallbackCheckInterval,
		logger:   log,
	}

	routes := router.GetRoutes()
	route := routes[model]
	endpoint := route.ActiveEndpoints[0]

	w := &fallbackWorker{
		service:   s,
		model:     model,
		provider:  endpoint.Provider.Name,
		config:    endpoint.Fallback,
		triggered: false,
	}

	expectedProviderName := "Eternis"
	provider, err := router.RouteModel(model, "")
	if err != nil {
		t.Fatalf("RouteModel failed: %v", err)
	}

	if provider.Name != expectedProviderName {
		t.Errorf("Expected initial provider %s, got %s", expectedProviderName, provider.Name)
	}

	api := newPromQueryAPIEmulator(t, endpoint.Fallback.Trigger.Query, 1)
	now := time.Now()
	expectedNextRunTime := now.Add(endpoint.Fallback.Trigger.DwellTime)
	nextRunTime := w.refreshEndpoints(api, now)

	if !nextRunTime.Equal(expectedNextRunTime) {
		t.Errorf("Expected next run time %v, got %v", expectedNextRunTime, nextRunTime)
	}

	expectedProviderName = "NEAR AI"
	provider, err = router.RouteModel(model, "")
	if err != nil {
		t.Fatalf("RouteModel failed: %v", err)
	}

	if provider.Name != expectedProviderName {
		t.Errorf("Expected provider %s, got %s", expectedProviderName, provider.Name)
	}
}

func TestRecoverTrigger(t *testing.T) {
	router := newModelRouter(t)

	s := &FallbackService{
		router:   router,
		interval: config.DefaultFallbackCheckInterval,
		logger:   log,
	}

	routes := router.GetRoutes()
	route := routes[model]
	endpoint := route.ActiveEndpoints[0]

	w := &fallbackWorker{
		service:   s,
		model:     model,
		provider:  endpoint.Provider.Name,
		config:    endpoint.Fallback,
		triggered: true,
	}

	route = routing.ModelRoute{
		ActiveEndpoints:   route.InactiveEndpoints,
		InactiveEndpoints: route.ActiveEndpoints,
		RoundRobinCounter: route.RoundRobinCounter,
	}
	routes[model] = route
	router.SetRoutes(routes)

	expectedProviderName := "NEAR AI"
	provider, err := router.RouteModel(model, "")
	if err != nil {
		t.Fatalf("RouteModel failed: %v", err)
	}

	if provider.Name != expectedProviderName {
		t.Errorf("Expected initial provider %s, got %s", expectedProviderName, provider.Name)
	}

	api := newPromQueryAPIEmulator(t, endpoint.Fallback.Recover.Query, 1)
	now := time.Now()
	expectedNextRunTime := now.Add(endpoint.Fallback.Recover.DwellTime)
	nextRunTime := w.refreshEndpoints(api, now)

	if !nextRunTime.Equal(expectedNextRunTime) {
		t.Errorf("Expected next run time %v, got %v", expectedNextRunTime, nextRunTime)
	}

	expectedProviderName = "Eternis"
	provider, err = router.RouteModel(model, "")
	if err != nil {
		t.Fatalf("RouteModel failed: %v", err)
	}

	if provider.Name != expectedProviderName {
		t.Errorf("Expected provider %s, got %s", expectedProviderName, provider.Name)
	}
}
