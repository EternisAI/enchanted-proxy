package health

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	_ "github.com/lib/pq"
)

// newTestDB opens a dummy postgres connection that will fail to ping.
// This avoids adding a test-only dependency.
func newUnreachableDB() *sql.DB {
	// Use an invalid DSN so Ping() fails without blocking long.
	db, _ := sql.Open("postgres", "host=127.0.0.1 port=1 connect_timeout=1 sslmode=disable")
	return db
}

func TestReadinessProbe_NotReadyBeforeStartup(t *testing.T) {
	// Even with an unreachable DB, startup check alone should cause 503.
	db := newUnreachableDB()
	defer db.Close()

	probe := NewReadinessProbe(db)
	handler := probe.Handler()

	req := httptest.NewRequest(http.MethodGet, "/healthz/ready", nil)
	rec := httptest.NewRecorder()

	handler(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503 before startup, got %d", rec.Code)
	}

	var resp readinessResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if resp.Ready {
		t.Error("expected ready=false before startup")
	}
	if resp.Checks["startup"] != "initializing" {
		t.Errorf("expected startup=initializing, got %s", resp.Checks["startup"])
	}
}

func TestReadinessProbe_ReadyAfterStartup_DBDown(t *testing.T) {
	db := newUnreachableDB()
	defer db.Close()

	probe := NewReadinessProbe(db)
	probe.MarkReady()

	handler := probe.Handler()

	req := httptest.NewRequest(http.MethodGet, "/healthz/ready", nil)
	rec := httptest.NewRecorder()

	handler(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503 when DB is unreachable, got %d", rec.Code)
	}

	var resp readinessResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if resp.Ready {
		t.Error("expected ready=false when DB is unreachable")
	}
	if resp.Checks["startup"] != "complete" {
		t.Errorf("expected startup=complete, got %s", resp.Checks["startup"])
	}
	if resp.Checks["database"] != "unreachable" {
		t.Errorf("expected database=unreachable, got %s", resp.Checks["database"])
	}
}

func TestReadinessProbe_MarkReady(t *testing.T) {
	db := newUnreachableDB()
	defer db.Close()

	probe := NewReadinessProbe(db)

	if probe.startupComplete.Load() {
		t.Error("expected startupComplete=false initially")
	}

	probe.MarkReady()

	if !probe.startupComplete.Load() {
		t.Error("expected startupComplete=true after MarkReady")
	}
}

func TestReadinessProbe_ResponseFormat(t *testing.T) {
	db := newUnreachableDB()
	defer db.Close()

	probe := NewReadinessProbe(db)
	handler := probe.Handler()

	req := httptest.NewRequest(http.MethodGet, "/healthz/ready", nil)
	rec := httptest.NewRecorder()

	handler(rec, req)

	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("expected Content-Type application/json, got %s", ct)
	}

	var resp readinessResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("response body should be valid JSON: %v", err)
	}

	if _, ok := resp.Checks["startup"]; !ok {
		t.Error("response should include startup check")
	}
	if _, ok := resp.Checks["database"]; !ok {
		t.Error("response should include database check")
	}
}
