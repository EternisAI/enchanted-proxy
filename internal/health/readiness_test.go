package health

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	_ "github.com/lib/pq"
)

// newUnreachableDB opens a dummy postgres connection that will fail to ping.
func newUnreachableDB() *sql.DB {
	db, _ := sql.Open("postgres", "host=127.0.0.1 port=1 connect_timeout=1 sslmode=disable")
	return db
}

// newProbeWithDBState creates a ReadinessProbe and sets the dbHealthy flag
// directly, avoiding the need for a real database or background goroutine.
func newProbeWithDBState(dbUp bool) *ReadinessProbe {
	db := newUnreachableDB()
	p := NewReadinessProbe(db)
	p.dbHealthy.Store(dbUp)
	return p
}

func TestReadinessProbe_NotReadyBeforeStartup(t *testing.T) {
	probe := newProbeWithDBState(true)
	defer probe.db.Close()

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
	probe := newProbeWithDBState(false)
	defer probe.db.Close()
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

func TestReadinessProbe_ReadyAfterStartup_DBUp(t *testing.T) {
	probe := newProbeWithDBState(true)
	defer probe.db.Close()
	probe.MarkReady()

	handler := probe.Handler()

	req := httptest.NewRequest(http.MethodGet, "/healthz/ready", nil)
	rec := httptest.NewRecorder()

	handler(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200 when fully ready, got %d", rec.Code)
	}

	var resp readinessResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if !resp.Ready {
		t.Error("expected ready=true")
	}
	if resp.Checks["startup"] != "complete" {
		t.Errorf("expected startup=complete, got %s", resp.Checks["startup"])
	}
	if resp.Checks["database"] != "ok" {
		t.Errorf("expected database=ok, got %s", resp.Checks["database"])
	}
}

func TestReadinessProbe_MarkReady(t *testing.T) {
	probe := newProbeWithDBState(false)
	defer probe.db.Close()

	if probe.startupComplete.Load() {
		t.Error("expected startupComplete=false initially")
	}

	probe.MarkReady()

	if !probe.startupComplete.Load() {
		t.Error("expected startupComplete=true after MarkReady")
	}
}

func TestReadinessProbe_ResponseFormat(t *testing.T) {
	probe := newProbeWithDBState(false)
	defer probe.db.Close()

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

func TestReadinessProbe_CheckDB(t *testing.T) {
	db := newUnreachableDB()
	defer db.Close()

	probe := NewReadinessProbe(db)

	// Initially false
	if probe.dbHealthy.Load() {
		t.Error("expected dbHealthy=false initially")
	}

	// After checkDB with unreachable DB, should remain false
	probe.checkDB()
	if probe.dbHealthy.Load() {
		t.Error("expected dbHealthy=false after checkDB with unreachable DB")
	}
}

func TestReadinessProbe_StartStop(t *testing.T) {
	db := newUnreachableDB()
	defer db.Close()

	probe := NewReadinessProbe(db)
	probe.Start()
	probe.Stop() // should not panic or hang
}
