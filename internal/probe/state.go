package probe

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	bolt "go.etcd.io/bbolt"

	"github.com/eternisai/enchanted-proxy/internal/logger"
)

// Persistent probe state lets the prober survive restarts without losing track of
// which endpoints were already known to be failing. Without it, every restart —
// including the deliberate ones used to roll a rotated API key — re-enters the
// initial-state stage, where the first success is silent by design, so the
// recovery notification for the very outage that prompted the restart is lost.
//
// The store keeps exactly one record per probe target: its current state, when
// that state was entered, and when it was last probed. Probe attempts are not
// journalled; the record is updated in place.
const (
	// stateSchemaVersion is stamped into every record. Records carrying any other
	// version are ignored on load rather than migrated — the state is a cache that
	// can always be rebuilt by probing, so there is nothing worth migrating.
	stateSchemaVersion = 1

	// stateBucket holds one key per probe target (see targetKey.storageKey).
	stateBucket = "probe_state"

	// stateDebugPath serves the running prober's state. bbolt's lock is held for
	// the lifetime of the process that opened the database, so a second process
	// cannot read it — the live view has to come from inside.
	stateDebugPath = "/debug/state"

	// defaultStateFlushInterval is how often pending last-probe timestamps are
	// coalesced into a single write transaction.
	defaultStateFlushInterval = 5 * time.Minute

	// stateFutureSkew is how far ahead of now a persisted timestamp may sit before
	// the record is treated as corrupt. Allows for modest clock skew between the
	// writer and the reader of the volume.
	stateFutureSkew = 5 * time.Minute
)

// stateOpenTimeout bounds how long we wait for bbolt's file lock. A rolling
// restart on an RWO volume can briefly overlap two pods; rather than block
// startup indefinitely we give up and run without persistence. It is a variable
// only so tests do not have to sit out the full wait.
var stateOpenTimeout = 10 * time.Second

// healthState is the persisted health of a probe target.
type healthState string

const (
	stateHealthy healthState = "healthy"
	stateFailing healthState = "failing"
)

// valid reports whether s is a state this version knows how to act on.
func (s healthState) valid() bool {
	return s == stateHealthy || s == stateFailing
}

// healthStateFor maps a boolean health flag to its persisted representation.
func healthStateFor(healthy bool) healthState {
	if healthy {
		return stateHealthy
	}
	return stateFailing
}

// targetKey identifies a probe target across restarts. It mirrors the worker
// identity established in NewProbeService: the (base_url, effective_model) pair
// that workers are deduplicated on, plus the provider and canonical model names
// that label the target's metrics.
//
// Using the full tuple means a config change that reroutes a model to a different
// provider or upstream model name starts that target fresh instead of inheriting
// the state of the endpoint it replaced — the old record is simply never looked
// up again. That is the intended trade-off: it is a different endpoint.
type targetKey struct {
	Provider       string
	CanonicalModel string
	BaseURL        string
	EffectiveModel string
}

// newTargetKey builds a key, normalizing the base URL the same way the worker
// dedup key does so a trailing slash in config does not orphan existing state.
func newTargetKey(provider, canonicalModel, baseURL, effectiveModel string) targetKey {
	return targetKey{
		Provider:       provider,
		CanonicalModel: canonicalModel,
		BaseURL:        strings.TrimRight(baseURL, "/"),
		EffectiveModel: effectiveModel,
	}
}

// storageKey renders the key for use as a bbolt key. NUL is used as the separator
// because it cannot occur in any of the components, so the encoding is injective.
func (k targetKey) storageKey() []byte {
	return []byte(strings.Join([]string{
		k.Provider, k.CanonicalModel, k.BaseURL, k.EffectiveModel,
	}, "\x00"))
}

// logAttrs returns the key's components as log attributes.
func (k targetKey) logAttrs() []any {
	return []any{
		slog.String("provider", k.Provider),
		slog.String("model", k.CanonicalModel),
		slog.String("base_url", k.BaseURL),
		slog.String("effective_model", k.EffectiveModel),
	}
}

// targetState is the persisted record for one probe target. The identity fields
// duplicate the key so a record can be validated against the key it was stored
// under, and so a raw dump of the store is self-describing.
type targetState struct {
	Version        int         `json:"v"`
	Provider       string      `json:"provider"`
	CanonicalModel string      `json:"canonical_model"`
	BaseURL        string      `json:"base_url"`
	EffectiveModel string      `json:"effective_model"`
	State          healthState `json:"state"`
	StateChangedAt time.Time   `json:"state_changed_at"`
	LastProbeAt    time.Time   `json:"last_probe_at,omitzero"`
}

// key reconstructs the target key from the record's identity fields.
func (s targetState) key() targetKey {
	return targetKey{
		Provider:       s.Provider,
		CanonicalModel: s.CanonicalModel,
		BaseURL:        s.BaseURL,
		EffectiveModel: s.EffectiveModel,
	}
}

// validate reports why a record cannot be trusted, or nil if it can. Callers
// discard invalid records rather than failing: a target with no usable state
// simply starts as if it had never been probed.
func (s targetState) validate(now time.Time) error {
	if s.Version != stateSchemaVersion {
		return fmt.Errorf("unsupported schema version %d (want %d)", s.Version, stateSchemaVersion)
	}
	if !s.State.valid() {
		return fmt.Errorf("unknown state %q", s.State)
	}
	if s.Provider == "" || s.CanonicalModel == "" || s.BaseURL == "" || s.EffectiveModel == "" {
		return errors.New("incomplete target identity")
	}
	if s.StateChangedAt.IsZero() {
		return errors.New("missing state_changed_at")
	}
	if s.StateChangedAt.After(now.Add(stateFutureSkew)) {
		return fmt.Errorf("state_changed_at %s is in the future", s.StateChangedAt.Format(time.RFC3339))
	}
	if !s.LastProbeAt.IsZero() && s.LastProbeAt.After(now.Add(stateFutureSkew)) {
		return fmt.Errorf("last_probe_at %s is in the future", s.LastProbeAt.Format(time.RFC3339))
	}
	return nil
}

// stateStore persists probe state to a bbolt database on disk.
//
// State transitions are written through immediately, in their own transaction:
// they are rare, and losing one to an ungraceful shutdown would resurrect the
// very bug this store exists to fix. Last-probe timestamps are held in memory and
// coalesced into a single transaction per flush interval, so write volume stays
// flat regardless of how many endpoints are configured or how fast they are being
// retried.
//
// A nil *stateStore is a fully functional no-op, so callers never need to branch
// on whether persistence is configured.
type stateStore struct {
	db     *bolt.DB
	logger *logger.Logger

	mu    sync.Mutex
	dirty map[string]time.Time // storage key -> pending last-probe timestamp

	flushInterval time.Duration
	stop          chan struct{}
	done          chan struct{}
}

// openStateStore opens (creating if absent) the state database at path.
//
// Every failure mode here is non-fatal: a missing file, a missing directory, a
// read-only volume, a lock held by an outgoing pod, or an unreadable database all
// produce a warning and a nil store, and the prober runs exactly as it did before
// persistence existed. Losing restart continuity is never a reason to stop
// monitoring.
func openStateStore(path string, flushInterval time.Duration, log *logger.Logger) *stateStore {
	if path == "" {
		log.Info("persistent probe state disabled (no state database path configured)")
		return nil
	}

	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			log.Warn("persistent probe state unavailable: cannot create state directory",
				slog.String("path", path),
				slog.String("error", err.Error()))
			return nil
		}
	}

	existed := true
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		existed = false
	}

	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: stateOpenTimeout})
	if err != nil {
		log.Warn("persistent probe state unavailable: cannot open state database",
			slog.String("path", path),
			slog.String("error", err.Error()))
		return nil
	}

	if err := db.Update(func(tx *bolt.Tx) error {
		_, err := tx.CreateBucketIfNotExists([]byte(stateBucket))
		return err
	}); err != nil {
		log.Warn("persistent probe state unavailable: cannot initialize state database",
			slog.String("path", path),
			slog.String("error", err.Error()))
		_ = db.Close()
		return nil
	}

	if flushInterval <= 0 {
		flushInterval = defaultStateFlushInterval
	}

	s := &stateStore{
		db:            db,
		logger:        log,
		dirty:         make(map[string]time.Time),
		flushInterval: flushInterval,
		stop:          make(chan struct{}),
		done:          make(chan struct{}),
	}

	if existed {
		log.Info("opened persistent probe state",
			slog.String("path", path),
			slog.Duration("flush_interval", flushInterval))
	} else {
		log.Warn("no persistent probe state found, starting fresh",
			slog.String("path", path))
	}

	go s.flushLoop()

	return s
}

// Load returns every valid record in the store, keyed by target key. Records that
// fail to decode, carry an unknown schema version, or disagree with the key they
// are stored under are skipped with a warning: garbage is ignored, never removed,
// and never fatal.
func (s *stateStore) Load() map[targetKey]targetState {
	if s == nil {
		return nil
	}

	now := time.Now()
	loaded := make(map[targetKey]targetState)
	invalid := 0

	err := s.db.View(func(tx *bolt.Tx) error {
		bucket := tx.Bucket([]byte(stateBucket))
		if bucket == nil {
			return nil
		}
		return bucket.ForEach(func(k, v []byte) error {
			var record targetState
			if err := json.Unmarshal(v, &record); err != nil {
				invalid++
				s.logger.Warn("ignoring unreadable probe state record",
					slog.String("key", strings.ReplaceAll(string(k), "\x00", "|")),
					slog.String("error", err.Error()))
				return nil
			}
			if err := record.validate(now); err != nil {
				invalid++
				s.logger.Warn("ignoring invalid probe state record",
					slog.String("key", strings.ReplaceAll(string(k), "\x00", "|")),
					slog.String("error", err.Error()))
				return nil
			}
			// The identity inside the record must agree with the key it lives
			// under; a mismatch means the file was written by something else.
			key := record.key()
			if string(key.storageKey()) != string(k) {
				invalid++
				s.logger.Warn("ignoring probe state record whose identity does not match its key",
					slog.String("key", strings.ReplaceAll(string(k), "\x00", "|")))
				return nil
			}
			loaded[key] = record
			return nil
		})
	})
	if err != nil {
		s.logger.Warn("failed to read persistent probe state, starting fresh",
			slog.String("error", err.Error()))
		return nil
	}

	s.logger.Info("loaded persistent probe state",
		slog.Int("records", len(loaded)),
		slog.Int("ignored", invalid))

	return loaded
}

// RecordStateChange durably persists a health transition for key. The write goes
// to disk immediately rather than waiting for the next flush, because a state
// change lost to an ungraceful shutdown is exactly the failure this store exists
// to prevent. Any pending last-probe timestamp for the target is folded into the
// same write.
func (s *stateStore) RecordStateChange(key targetKey, state healthState, at time.Time) {
	if s == nil {
		return
	}

	record := targetState{
		Version:        stateSchemaVersion,
		Provider:       key.Provider,
		CanonicalModel: key.CanonicalModel,
		BaseURL:        key.BaseURL,
		EffectiveModel: key.EffectiveModel,
		State:          state,
		StateChangedAt: at,
		LastProbeAt:    at,
	}

	storageKey := key.storageKey()

	s.mu.Lock()
	delete(s.dirty, string(storageKey))
	s.mu.Unlock()

	if err := s.put(record); err != nil {
		s.logger.Warn("failed to persist probe state change",
			append(key.logAttrs(), slog.String("error", err.Error()))...)
		return
	}

	s.logger.Debug("persisted probe state change",
		append(key.logAttrs(), slog.String("state", string(state)))...)
}

// RecordProbe notes that key was probed at the given time. The timestamp is held
// in memory and written by the next flush; see the type comment for why this is
// not written through.
func (s *stateStore) RecordProbe(key targetKey, at time.Time) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.dirty[string(key.storageKey())] = at
	s.mu.Unlock()
}

// put writes a single record in its own transaction.
func (s *stateStore) put(record targetState) error {
	encoded, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("encoding probe state: %w", err)
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		bucket, err := tx.CreateBucketIfNotExists([]byte(stateBucket))
		if err != nil {
			return err
		}
		return bucket.Put(record.key().storageKey(), encoded)
	})
}

// Flush writes all pending last-probe timestamps in a single transaction.
//
// A timestamp is only applied to a target that already has a valid record: the
// first thing a worker persists is its state, so a missing record means the
// target has not established a state yet and there is nothing for a bare
// timestamp to attach to.
func (s *stateStore) Flush() {
	if s == nil {
		return
	}

	s.mu.Lock()
	if len(s.dirty) == 0 {
		s.mu.Unlock()
		return
	}
	pending := s.dirty
	s.dirty = make(map[string]time.Time)
	s.mu.Unlock()

	written := 0
	err := s.db.Update(func(tx *bolt.Tx) error {
		bucket, err := tx.CreateBucketIfNotExists([]byte(stateBucket))
		if err != nil {
			return err
		}
		for storageKey, at := range pending {
			raw := bucket.Get([]byte(storageKey))
			if raw == nil {
				continue
			}
			var record targetState
			if err := json.Unmarshal(raw, &record); err != nil {
				continue
			}
			record.LastProbeAt = at
			encoded, err := json.Marshal(record)
			if err != nil {
				continue
			}
			if err := bucket.Put([]byte(storageKey), encoded); err != nil {
				return err
			}
			written++
		}
		return nil
	})
	if err != nil {
		// The timestamps are a scheduling optimization, not correctness state —
		// drop them rather than retrying and growing the pending set unbounded.
		s.logger.Warn("failed to flush probe state timestamps",
			slog.Int("pending", len(pending)),
			slog.String("error", err.Error()))
		return
	}

	s.logger.Debug("flushed probe state timestamps", slog.Int("records", written))
}

// flushLoop coalesces pending timestamp writes until the store is closed.
func (s *stateStore) flushLoop() {
	defer close(s.done)

	ticker := time.NewTicker(s.flushInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			s.Flush()
		case <-s.stop:
			return
		}
	}
}

// Close stops the flush loop, writes any pending timestamps, and closes the
// database. It is safe to call on a nil store.
func (s *stateStore) Close() {
	if s == nil {
		return
	}
	close(s.stop)
	<-s.done
	s.Flush()
	if err := s.db.Close(); err != nil {
		s.logger.Warn("failed to close persistent probe state",
			slog.String("error", err.Error()))
	}
}
