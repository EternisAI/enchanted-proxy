package health

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

const (
	dbCheckInterval = 5 * time.Second
	dbCheckTimeout  = 2 * time.Second
)

// ReadinessProbe tracks whether the server is ready to accept traffic.
// It combines a one-shot startup gate with an async database health check
// that runs in a background goroutine, so the HTTP handler never blocks
// on dependency latency.
type ReadinessProbe struct {
	startupComplete atomic.Bool
	dbHealthy       atomic.Bool
	db              *sql.DB
	stop            chan struct{}
	stopOnce        sync.Once
}

// NewReadinessProbe creates a new readiness probe that checks the given database.
// Call Start() to begin the background health check loop.
func NewReadinessProbe(db *sql.DB) *ReadinessProbe {
	return &ReadinessProbe{
		db:   db,
		stop: make(chan struct{}),
	}
}

// Start launches the background goroutine that periodically pings the
// database and updates the atomic health status. It performs an immediate
// check before entering the ticker loop.
func (p *ReadinessProbe) Start() {
	// Immediate check so the probe has a valid state before the first tick.
	p.checkDB()

	go func() {
		ticker := time.NewTicker(dbCheckInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				p.checkDB()
			case <-p.stop:
				return
			}
		}
	}()
}

// Stop terminates the background health check goroutine and marks the probe as
// not ready so /healthz/ready immediately returns 503. Safe to call multiple times.
func (p *ReadinessProbe) Stop() {
	p.stopOnce.Do(func() {
		p.startupComplete.Store(false)
		p.dbHealthy.Store(false)
		close(p.stop)
	})
}

func (p *ReadinessProbe) checkDB() {
	ctx, cancel := context.WithTimeout(context.Background(), dbCheckTimeout)
	defer cancel()
	p.dbHealthy.Store(p.db.PingContext(ctx) == nil)
}

// MarkReady signals that all startup initialization is complete and
// HTTP servers have been launched.
func (p *ReadinessProbe) MarkReady() {
	p.startupComplete.Store(true)
}

type readinessResponse struct {
	Ready  bool              `json:"ready"`
	Checks map[string]string `json:"checks"`
}

// Handler returns an http.HandlerFunc for the /healthz/ready endpoint.
// Returns 200 when the server is ready, 503 otherwise.
// The handler only reads atomic values — it never blocks on I/O.
func (p *ReadinessProbe) Handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		resp := readinessResponse{
			Ready:  true,
			Checks: make(map[string]string),
		}

		// Check 1: startup complete
		if !p.startupComplete.Load() {
			resp.Ready = false
			resp.Checks["startup"] = "initializing"
		} else {
			resp.Checks["startup"] = "complete"
		}

		// Check 2: database reachable (async — reads last-known state)
		if !p.dbHealthy.Load() {
			resp.Ready = false
			resp.Checks["database"] = "unreachable"
		} else {
			resp.Checks["database"] = "ok"
		}

		w.Header().Set("Content-Type", "application/json")
		if resp.Ready {
			w.WriteHeader(http.StatusOK)
		} else {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		json.NewEncoder(w).Encode(resp)
	}
}
