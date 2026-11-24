package task

import (
	"database/sql"
	"log/slog"
	"testing"

	"github.com/eternisai/enchanted-proxy/internal/logger"
	_ "github.com/lib/pq"
)

// A lock holder keeps one connection for the lock and needs another for its
// work, so a pool of one connection could only time out.
func TestTaskLocksRefuseAPoolTooSmallForTheirWork(t *testing.T) {
	for size, wantErr := range map[int]bool{0: false, 1: true, 2: false, 10: false} {
		db, err := sql.Open("postgres", "postgres://unused") // does not connect
		if err != nil {
			t.Fatal(err)
		}
		db.SetMaxOpenConns(size) // 0 is no limit
		_, err = lockedTasks(db, logger.New(logger.Config{Level: slog.LevelError + 4}))
		if (err != nil) != wantErr {
			t.Errorf("pool of %d: err = %v, want an error %v", size, err, wantErr)
		}
		_ = db.Close()
	}
}
