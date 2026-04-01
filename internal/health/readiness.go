package health

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"sync/atomic"
	"time"
)

// ReadinessProbe tracks whether the server is ready to accept traffic.
// It combines a one-shot startup gate with ongoing dependency health checks.
type ReadinessProbe struct {
	startupComplete atomic.Bool
	db              *sql.DB
}

// NewReadinessProbe creates a new readiness probe that checks the given database.
func NewReadinessProbe(db *sql.DB) *ReadinessProbe {
	return &ReadinessProbe{db: db}
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

		// Check 2: database reachable
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()

		if err := p.db.PingContext(ctx); err != nil {
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
