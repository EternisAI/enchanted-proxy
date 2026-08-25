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

// dumpRecord is one entry in the -dump-state output. Records that would be
// ignored at load time are included too, flagged with the reason, so a dump can
// be used to diagnose a store the prober is silently skipping over.
type dumpRecord struct {
	Key   string       `json:"key"`
	Valid bool         `json:"valid"`
	Error string       `json:"error,omitempty"`
	State *targetState `json:"state,omitempty"`
	Raw   string       `json:"raw,omitempty"`
}

// DumpState writes the contents of the state database at path to w as JSON.
// The database is opened read-only, so a running prober is unaffected.
func DumpState(path string, w io.Writer) error {
	if path == "" {
		return errors.New("no state database path configured")
	}
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("state database %s: %w", path, err)
	}

	db, err := bolt.Open(path, 0o600, &bolt.Options{ReadOnly: true, Timeout: stateOpenTimeout})
	if err != nil {
		return fmt.Errorf("opening state database %s: %w", path, err)
	}
	defer db.Close()

	now := time.Now()
	records := make([]dumpRecord, 0)

	err = db.View(func(tx *bolt.Tx) error {
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

			switch {
			case record.validate(now) != nil:
				entry.Error = record.validate(now).Error()
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
		return fmt.Errorf("reading state database: %w", err)
	}

	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	return encoder.Encode(records)
}
