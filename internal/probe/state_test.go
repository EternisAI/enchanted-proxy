package probe

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"

	"github.com/eternisai/enchanted-proxy/internal/logger"
)

// testLogger returns a logger that suppresses everything the store emits, so
// test output stays readable.
func testLogger() *logger.Logger {
	return logger.New(logger.Config{Level: slog.LevelError + 1, Format: "json"})
}

func testKey() targetKey {
	return newTargetKey("OpenAI", "openai/gpt-5.5", "https://api.openai.com/v1", "gpt-5.5")
}

// openTestStore opens a store in a temp dir with a flush interval long enough
// that the background loop never fires during the test.
func openTestStore(t *testing.T, path string) *stateStore {
	t.Helper()
	store := openStateStore(path, time.Hour, testLogger())
	if store == nil {
		t.Fatal("openStateStore returned nil for a usable path")
	}
	t.Cleanup(store.Close)
	return store
}

func TestTargetKeyNormalizesBaseURL(t *testing.T) {
	withSlash := newTargetKey("OpenAI", "openai/gpt-5.5", "https://api.openai.com/v1/", "gpt-5.5")
	withoutSlash := newTargetKey("OpenAI", "openai/gpt-5.5", "https://api.openai.com/v1", "gpt-5.5")

	if withSlash != withoutSlash {
		t.Errorf("trailing slash changed the key: %+v vs %+v", withSlash, withoutSlash)
	}
	if string(withSlash.storageKey()) != string(withoutSlash.storageKey()) {
		t.Error("trailing slash changed the storage key")
	}
}

func TestTargetKeyStorageKeyIsInjective(t *testing.T) {
	// Components that would collide under a naive delimiter-free concatenation.
	a := newTargetKey("a", "bc", "https://x", "y")
	b := newTargetKey("ab", "c", "https://x", "y")

	if string(a.storageKey()) == string(b.storageKey()) {
		t.Errorf("distinct targets produced the same storage key: %q", a.storageKey())
	}
}

func TestStateStoreRoundTripAcrossRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	key := testKey()
	changedAt := time.Now().Add(-30 * time.Minute).Truncate(time.Second)

	store := openStateStore(path, time.Hour, testLogger())
	if store == nil {
		t.Fatal("openStateStore returned nil")
	}
	store.RecordStateChange(key, stateFailing, changedAt)
	store.Close()

	// Reopen, as a restarted pod would.
	reopened := openTestStore(t, path)
	loaded := reopened.Load()

	record, ok := loaded[key]
	if !ok {
		t.Fatalf("state for %+v was not restored; got %d records", key, len(loaded))
	}
	if record.State != stateFailing {
		t.Errorf("state = %q, want %q", record.State, stateFailing)
	}
	if !record.StateChangedAt.Equal(changedAt) {
		t.Errorf("state_changed_at = %s, want %s", record.StateChangedAt, changedAt)
	}
	if !record.LastProbeAt.Equal(changedAt) {
		t.Errorf("last_probe_at = %s, want %s (state changes stamp both)", record.LastProbeAt, changedAt)
	}
}

func TestStateStoreMissingDatabaseIsNotAnError(t *testing.T) {
	// A fresh volume: the directory exists but holds no database yet.
	path := filepath.Join(t.TempDir(), "state.db")

	store := openTestStore(t, path)
	if got := store.Load(); len(got) != 0 {
		t.Errorf("expected no records from a fresh database, got %d", len(got))
	}
}

func TestStateStoreCreatesMissingDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "dir", "state.db")

	store := openTestStore(t, path)
	store.RecordStateChange(testKey(), stateHealthy, time.Now())

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("database was not created at %s: %v", path, err)
	}
}

func TestStateStoreDisabledWhenPathEmpty(t *testing.T) {
	if store := openStateStore("", time.Hour, testLogger()); store != nil {
		t.Fatal("expected a nil store when no path is configured")
	}
}

func TestStateStoreUnusablePathDegradesToNil(t *testing.T) {
	// A file where a directory needs to be: MkdirAll fails, and the prober must
	// carry on without persistence rather than refusing to start.
	blocker := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	if store := openStateStore(filepath.Join(blocker, "state.db"), time.Hour, testLogger()); store != nil {
		store.Close()
		t.Fatal("expected a nil store for an unusable path")
	}
}

func TestNilStoreIsANoOp(t *testing.T) {
	var store *stateStore

	// Every call site treats persistence as optional; none of these may panic.
	if got := store.Load(); got != nil {
		t.Errorf("nil store Load() = %v, want nil", got)
	}
	store.RecordStateChange(testKey(), stateHealthy, time.Now())
	store.RecordProbe(testKey(), time.Now())
	store.Flush()
	store.Close()
}

func TestStateStoreIgnoresInvalidRecords(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	valid := testKey()

	store := openTestStore(t, path)
	store.RecordStateChange(valid, stateHealthy, time.Now().Add(-time.Hour))

	mismatched := targetState{
		Version:        stateSchemaVersion,
		Provider:       "Tinfoil",
		CanonicalModel: "meta-llama/Llama-3.3-70B",
		BaseURL:        "https://inference.tinfoil.sh/v1",
		EffectiveModel: "llama3-3-70b",
		State:          stateHealthy,
		StateChangedAt: time.Now().Add(-time.Hour),
	}

	corrupt := map[string]any{
		"not JSON at all": "definitely-not-json",
		"unknown schema version": targetState{
			Version: 99, Provider: "P", CanonicalModel: "m", BaseURL: "u", EffectiveModel: "e",
			State: stateHealthy, StateChangedAt: time.Now().Add(-time.Hour),
		},
		"unknown state": targetState{
			Version: stateSchemaVersion, Provider: "P", CanonicalModel: "m", BaseURL: "u", EffectiveModel: "e",
			State: "somewhere-in-between", StateChangedAt: time.Now().Add(-time.Hour),
		},
		"missing identity": targetState{
			Version: stateSchemaVersion, State: stateHealthy, StateChangedAt: time.Now().Add(-time.Hour),
		},
		"zero timestamp": targetState{
			Version: stateSchemaVersion, Provider: "P", CanonicalModel: "m", BaseURL: "u", EffectiveModel: "e",
			State: stateHealthy,
		},
		"timestamp from the future": targetState{
			Version: stateSchemaVersion, Provider: "P", CanonicalModel: "m", BaseURL: "u", EffectiveModel: "e",
			State: stateHealthy, StateChangedAt: time.Now().Add(24 * time.Hour),
		},
	}

	// Write the bad rows behind the store's back, as a corrupted volume would.
	err := store.db.Update(func(tx *bolt.Tx) error {
		bucket := tx.Bucket([]byte(stateBucket))
		for name, value := range corrupt {
			var encoded []byte
			if raw, ok := value.(string); ok {
				encoded = []byte(raw)
			} else {
				encoded, _ = json.Marshal(value)
			}
			if err := bucket.Put([]byte("corrupt\x00"+name), encoded); err != nil {
				return err
			}
		}
		// A record whose identity disagrees with the key it is stored under.
		encoded, _ := json.Marshal(mismatched)
		return bucket.Put([]byte("some\x00other\x00key\x00entirely"), encoded)
	})
	if err != nil {
		t.Fatal(err)
	}

	loaded := store.Load()

	if len(loaded) != 1 {
		t.Fatalf("loaded %d records, want only the 1 valid one: %+v", len(loaded), loaded)
	}
	if _, ok := loaded[valid]; !ok {
		t.Errorf("the valid record was not loaded; got %+v", loaded)
	}

	// Ignored, not deleted — no garbage collection.
	var stored int
	_ = store.db.View(func(tx *bolt.Tx) error {
		stored = tx.Bucket([]byte(stateBucket)).Stats().KeyN
		return nil
	})
	if want := len(corrupt) + 2; stored != want {
		t.Errorf("store holds %d keys, want %d (invalid rows must be ignored, not removed)", stored, want)
	}
}

func TestStateStoreCoalescesProbeTimestamps(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	key := testKey()
	changedAt := time.Now().Add(-time.Hour).Truncate(time.Second)

	store := openTestStore(t, path)
	store.RecordStateChange(key, stateHealthy, changedAt)

	// Repeated probes must not each hit the disk.
	first := time.Now().Add(-30 * time.Minute).Truncate(time.Second)
	last := time.Now().Add(-1 * time.Minute).Truncate(time.Second)
	store.RecordProbe(key, first)
	store.RecordProbe(key, last)

	if got := store.Load()[key].LastProbeAt; !got.Equal(changedAt) {
		t.Errorf("last_probe_at = %s before flush, want the unflushed %s", got, changedAt)
	}

	store.Flush()

	if got := store.Load()[key].LastProbeAt; !got.Equal(last) {
		t.Errorf("last_probe_at = %s after flush, want %s", got, last)
	}
	// The flush must not disturb the state it is attached to.
	if got := store.Load()[key]; got.State != stateHealthy || !got.StateChangedAt.Equal(changedAt) {
		t.Errorf("flush altered the state record: %+v", got)
	}
}

func TestStateChangeSupersedesPendingTimestamp(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	key := testKey()

	store := openTestStore(t, path)
	store.RecordStateChange(key, stateHealthy, time.Now().Add(-time.Hour))

	// A probe that turns out to be a state change: the pending timestamp must not
	// survive to overwrite the newer one written by the transition.
	stale := time.Now().Add(-time.Hour).Truncate(time.Second)
	store.RecordProbe(key, stale)

	changedAt := time.Now().Truncate(time.Second)
	store.RecordStateChange(key, stateFailing, changedAt)
	store.Flush()

	record := store.Load()[key]
	if !record.LastProbeAt.Equal(changedAt) {
		t.Errorf("last_probe_at = %s, want %s (stale pending timestamp leaked through)", record.LastProbeAt, changedAt)
	}
	if record.State != stateFailing {
		t.Errorf("state = %q, want %q", record.State, stateFailing)
	}
}

func TestFlushIgnoresTimestampsForUnknownTargets(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")

	store := openTestStore(t, path)
	// A worker that has probed but not yet established a state has nothing for a
	// bare timestamp to attach to.
	store.RecordProbe(testKey(), time.Now())
	store.Flush()

	if got := store.Load(); len(got) != 0 {
		t.Errorf("flush created %d records from timestamps alone, want 0: %+v", len(got), got)
	}
}

func TestCloseFlushesPendingTimestamps(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	key := testKey()
	probedAt := time.Now().Truncate(time.Second)

	store := openStateStore(path, time.Hour, testLogger())
	if store == nil {
		t.Fatal("openStateStore returned nil")
	}
	store.RecordStateChange(key, stateHealthy, time.Now().Add(-time.Hour))
	store.RecordProbe(key, probedAt)
	store.Close() // graceful shutdown

	reopened := openTestStore(t, path)
	if got := reopened.Load()[key].LastProbeAt; !got.Equal(probedAt) {
		t.Errorf("last_probe_at = %s after graceful shutdown, want %s", got, probedAt)
	}
}

func TestDumpState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	key := testKey()

	store := openStateStore(path, time.Hour, testLogger())
	if store == nil {
		t.Fatal("openStateStore returned nil")
	}
	store.RecordStateChange(key, stateFailing, time.Now().Add(-time.Hour))
	err := store.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(stateBucket)).Put([]byte("junk"), []byte("{not json"))
	})
	if err != nil {
		t.Fatal(err)
	}
	store.Close()

	var out bytes.Buffer
	if err := DumpState(path, &out); err != nil {
		t.Fatalf("DumpState: %v", err)
	}

	var records []dumpRecord
	if err := json.Unmarshal(out.Bytes(), &records); err != nil {
		t.Fatalf("dump output is not valid JSON: %v\n%s", err, out.String())
	}
	if len(records) != 2 {
		t.Fatalf("dumped %d records, want 2: %s", len(records), out.String())
	}

	var valid, invalid int
	for _, record := range records {
		if record.Valid {
			valid++
			if record.State == nil || record.State.State != stateFailing {
				t.Errorf("valid record has unexpected state: %+v", record.State)
			}
		} else {
			invalid++
			if record.Error == "" {
				t.Error("invalid record was dumped without a reason")
			}
		}
	}
	if valid != 1 || invalid != 1 {
		t.Errorf("dump had %d valid / %d invalid records, want 1 / 1", valid, invalid)
	}
}

func TestDumpStateMissingDatabase(t *testing.T) {
	var out bytes.Buffer

	if err := DumpState(filepath.Join(t.TempDir(), "absent.db"), &out); err == nil {
		t.Error("expected an error when dumping a database that does not exist")
	}
	if err := DumpState("", &out); err == nil {
		t.Error("expected an error when dumping with no path configured")
	}
}

// TestSnapshotReadsWhileDatabaseIsOpen covers the case DumpState cannot: bbolt
// holds its file lock for the lifetime of the process that opened the database,
// so the live view has to be read through the open handle rather than by
// reopening the file.
func TestSnapshotReadsWhileDatabaseIsOpen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	key := testKey()

	store := openTestStore(t, path)
	store.RecordStateChange(key, stateFailing, time.Now().Add(-time.Hour))

	// Reopening the same path while the store holds it must fail; shorten the
	// lock wait so the test does not sit out the production timeout.
	defer func(orig time.Duration) { stateOpenTimeout = orig }(stateOpenTimeout)
	stateOpenTimeout = 200 * time.Millisecond

	var discard bytes.Buffer
	if err := DumpState(path, &discard); err == nil {
		t.Error("expected DumpState to fail against an open database")
	} else if !strings.Contains(err.Error(), "locked") {
		t.Errorf("expected a lock-specific message, got: %v", err)
	}

	// ...while the in-process snapshot succeeds.
	records, err := store.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("snapshot returned %d records, want 1", len(records))
	}
	if !records[0].Valid || records[0].State.State != stateFailing {
		t.Errorf("unexpected snapshot record: %+v", records[0])
	}
}

func TestSnapshotOnNilStore(t *testing.T) {
	var store *stateStore
	if _, err := store.Snapshot(); err == nil {
		t.Error("expected an error when persistence is disabled")
	}
}
