package probe

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	bolt "go.etcd.io/bbolt"
)

// dumpRecord is one entry in a state dump. Records that would be ignored at load
// time are included too, flagged with the reason, so a dump can be used to
// diagnose a store the prober is silently skipping over.
type dumpRecord struct {
	Key   string       `json:"key"`
	Valid bool         `json:"valid"`
	Error string       `json:"error,omitempty"`
	State *targetState `json:"state,omitempty"`
	Raw   string       `json:"raw,omitempty"`
}

// collectDumpRecords renders every record in the store's bucket, valid or not.
func collectDumpRecords(db *bolt.DB, now time.Time) ([]dumpRecord, error) {
	records := make([]dumpRecord, 0)

	err := db.View(func(tx *bolt.Tx) error {
		bucket := tx.Bucket([]byte(stateBucket))
		if bucket == nil {
			return nil
		}
		return bucket.ForEach(func(k, v []byte) error {
			entry := dumpRecord{Key: strings.ReplaceAll(string(k), "\x00", "|")}

			var record targetState
			if err := json.Unmarshal(v, &record); err != nil {
				entry.Error = err.Error()
				entry.Raw = string(v)
				records = append(records, entry)
				return nil
			}
			entry.State = &record

			switch validationErr := record.validate(now); {
			case validationErr != nil:
				entry.Error = validationErr.Error()
			case string(record.key().storageKey()) != string(k):
				entry.Error = "record identity does not match its key"
			default:
				entry.Valid = true
			}

			records = append(records, entry)
			return nil
		})
	})
	if err != nil {
		return nil, fmt.Errorf("reading state database: %w", err)
	}

	return records, nil
}

// Snapshot renders the current contents of the store. It reads through the
// already-open database, so it works while the prober is running — unlike
// DumpState, which has to take the file lock for itself.
func (s *stateStore) Snapshot() ([]dumpRecord, error) {
	if s == nil {
		return nil, errors.New("persistent probe state is not enabled")
	}
	return collectDumpRecords(s.db, time.Now())
}

// StateSnapshot renders the probe state held by this service, for the debug
// endpoint. Returns an error when persistence is disabled or unavailable.
func (s *ProbeService) StateSnapshot() ([]dumpRecord, error) {
	if s == nil {
		return nil, errors.New("probe service is not running")
	}
	return s.state.Snapshot()
}

// DumpState writes the contents of the state database at path to w as JSON.
//
// bbolt guards the file with an exclusive lock held for the lifetime of the
// process that opened it read-write, and a read-only open still needs a shared
// lock, so this cannot read the database of a running prober — use the debug
// endpoint for that. It is for inspecting a stopped prober's volume, or a copy
// of one.
func DumpState(path string, w io.Writer) error {
	if path == "" {
		return errors.New("no state database path configured")
	}
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("state database %s: %w", path, err)
	}

	db, err := bolt.Open(path, 0o600, &bolt.Options{ReadOnly: true, Timeout: stateOpenTimeout})
	if err != nil {
		if errors.Is(err, bolt.ErrTimeout) {
			return fmt.Errorf("state database %s is locked, most likely by a running prober: "+
				"read it from the live process at %s instead, or stop the prober first", path, stateDebugPath)
		}
		return fmt.Errorf("opening state database %s: %w", path, err)
	}
	defer db.Close()

	records, err := collectDumpRecords(db, time.Now())
	if err != nil {
		return err
	}

	return WriteDump(w, records)
}

// WriteDump encodes dump records as indented JSON.
func WriteDump(w io.Writer, records []dumpRecord) error {
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	return encoder.Encode(records)
}
