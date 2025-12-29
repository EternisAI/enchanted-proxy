package background

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/eternisai/enchanted-proxy/internal/config"
	"github.com/eternisai/enchanted-proxy/internal/logger"
	"github.com/eternisai/enchanted-proxy/internal/messaging"
	"github.com/eternisai/enchanted-proxy/internal/notifications"
	"github.com/eternisai/enchanted-proxy/internal/request_tracking"
	"log/slog"
)

// PollingManager manages background polling workers for GPT-5 Pro responses.
//
// Responsibilities:
//   - Track active polling workers
//   - Limit concurrent workers
//   - Cleanup completed workers
//   - Graceful shutdown
//
// Thread-safety: All methods are thread-safe.
type PollingManager struct {
	workers             map[string]context.CancelFunc // response_id → cancel function
	workersMu           sync.RWMutex
	messageService      *messaging.Service
	trackingService     *request_tracking.Service
	notificationService *notifications.Service
	logger              *logger.Logger
	cfg                 *config.Config
	shutdown            chan struct{}
	wg                  sync.WaitGroup
	activeCount         atomic.Int32
}

// NewPollingManager creates a new polling manager.
func NewPollingManager(
	messageService *messaging.Service,
	trackingService *request_tracking.Service,
	notificationService *notifications.Service,
	logger *logger.Logger,
	cfg *config.Config,
) *PollingManager {
	return &PollingManager{
		workers:             make(map[string]context.CancelFunc),
		messageService:      messageService,
		trackingService:     trackingService,
		notificationService: notificationService,
		logger:              logger.WithComponent("polling_manager"),
		cfg:                 cfg,
		shutdown:            make(chan struct{}),
	}
}

// StartPolling starts a background polling worker for a GPT-5 Pro response.
//
// This method is non-blocking - it spawns a goroutine that polls OpenAI
// and updates Firestore as status changes.
//
// Parameters:
//   - ctx: Context for the worker (cancellation)
//   - job: Polling job details
//   - apiKey: OpenAI API key for this request
//   - baseURL: OpenAI base URL
//   - tokenMultiplier: Cost multiplier for this model (e.g., 50× for GPT-5 Pro)
//
// Returns:
//   - error: If starting worker failed (e.g., too many workers)
func (pm *PollingManager) StartPolling(ctx context.Context, job PollingJob, apiKey, baseURL string, tokenMultiplier float64) error {
	// Check if already polling this response
	pm.workersMu.RLock()
	if _, exists := pm.workers[job.ResponseID]; exists {
		pm.workersMu.RUnlock()
		pm.logger.Warn("already polling this response",
			slog.String("response_id", job.ResponseID))
		return nil // Not an error, just ignore
	}
	pm.workersMu.RUnlock()

	// Check concurrent worker limit
	active := pm.activeCount.Load()
	if active >= int32(pm.cfg.BackgroundMaxConcurrentPolls) {
		pm.logger.Error("too many concurrent polling workers",
			slog.Int("active", int(active)),
			slog.Int("max", pm.cfg.BackgroundMaxConcurrentPolls))
		return fmt.Errorf("too many concurrent polling workers: %d/%d", active, pm.cfg.BackgroundMaxConcurrentPolls)
	}

	// Create worker context
	workerCtx, cancel := context.WithCancel(ctx)

	// Register worker
	pm.workersMu.Lock()
	pm.workers[job.ResponseID] = cancel
	pm.workersMu.Unlock()

	pm.activeCount.Add(1)

	// Spawn worker goroutine
	pm.wg.Add(1)
	go pm.runWorker(workerCtx, job, apiKey, baseURL, tokenMultiplier, cancel)

	pm.logger.Info("started background polling worker",
		slog.String("response_id", job.ResponseID),
		slog.Int("active_workers", int(pm.activeCount.Load())))

	return nil
}

// runWorker runs a polling worker in a goroutine.
func (pm *PollingManager) runWorker(ctx context.Context, job PollingJob, apiKey, baseURL string, tokenMultiplier float64, cancel context.CancelFunc) {
	defer pm.wg.Done()
	defer cancel()
	defer pm.activeCount.Add(-1)
	defer pm.unregisterWorker(job.ResponseID)

	startTime := time.Now()

	pm.logger.Info("polling worker goroutine started",
		slog.String("response_id", job.ResponseID),
		slog.Int("active_workers", int(pm.activeCount.Load())))

	// Create OpenAI client for this worker
	openAIClient := NewOpenAIClient(apiKey, baseURL, pm.logger)

	// Create worker with tracking service, notification service, and multiplier
	worker := NewPollingWorker(job, openAIClient, pm.messageService, pm.trackingService, pm.notificationService, pm.logger, pm.cfg, tokenMultiplier)

	// Run worker (blocks until done)
	if err := worker.Run(ctx); err != nil {
		pm.logger.Error("polling worker exited with error",
			slog.String("response_id", job.ResponseID),
			slog.String("error", err.Error()),
			slog.Duration("total_duration", time.Since(startTime)),
			slog.Int("remaining_workers", int(pm.activeCount.Load())-1))
		// Error already logged and saved by worker
	} else {
		pm.logger.Info("polling worker exited successfully",
			slog.String("response_id", job.ResponseID),
			slog.Duration("total_duration", time.Since(startTime)),
			slog.Int("remaining_workers", int(pm.activeCount.Load())-1))
	}
}

// unregisterWorker removes a worker from the registry.
func (pm *PollingManager) unregisterWorker(responseID string) {
	pm.workersMu.Lock()
	delete(pm.workers, responseID)
	pm.workersMu.Unlock()

	pm.logger.Debug("unregistered polling worker",
		slog.String("response_id", responseID),
		slog.Int("active_workers", int(pm.activeCount.Load())))
}

// CancelPolling cancels a specific polling worker.
//
// This can be used if the user cancels a request or if we need to stop
// polling for other reasons.
//
// Parameters:
//   - responseID: The response ID to cancel
func (pm *PollingManager) CancelPolling(responseID string) {
	pm.workersMu.RLock()
	cancel, exists := pm.workers[responseID]
	pm.workersMu.RUnlock()

	if exists {
		pm.logger.Info("cancelling polling worker",
			slog.String("response_id", responseID))
		cancel()
	}
}

// GetActiveCount returns the number of active polling workers.
func (pm *PollingManager) GetActiveCount() int {
	return int(pm.activeCount.Load())
}

// Shutdown gracefully shuts down the polling manager.
//
// This waits for all active workers to complete or timeout (30 seconds).
//
// Returns:
//   - error: If shutdown failed
func (pm *PollingManager) Shutdown() error {
	pm.logger.Info("shutting down polling manager",
		slog.Int("active_workers", int(pm.activeCount.Load())))

	close(pm.shutdown)

	// Cancel all workers
	pm.workersMu.Lock()
	for responseID, cancel := range pm.workers {
		pm.logger.Debug("cancelling worker during shutdown",
			slog.String("response_id", responseID))
		cancel()
	}
	pm.workersMu.Unlock()

	// Wait for all workers to finish (with timeout)
	done := make(chan struct{})
	go func() {
		pm.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		pm.logger.Info("all polling workers shut down successfully")
		return nil
	case <-time.After(30 * time.Second):
		pm.logger.Warn("polling manager shutdown timed out, some workers may still be running")
		return fmt.Errorf("shutdown timeout after 30 seconds")
	}
}

// GetWorkerStatus returns debug information about active workers.
//
// This is useful for monitoring and debugging.
//
// Returns:
//   - map[string]string: response_id → status
func (pm *PollingManager) GetWorkerStatus() map[string]string {
	pm.workersMu.RLock()
	defer pm.workersMu.RUnlock()

	status := make(map[string]string, len(pm.workers))
	for responseID := range pm.workers {
		status[responseID] = "polling"
	}
	return status
}
