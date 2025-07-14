package pg

import (
	"database/sql"
	"fmt"

	pgdb "github.com/eternisai/enchanted-proxy/pkg/storage/pg/sqlc"
	_ "github.com/lib/pq"
)

type Database struct {
	DB      *sql.DB
	Queries *pgdb.Queries
}

// InitDatabase initializes the database connection and runs migrations.
func InitDatabase(databaseURL string) (*Database, error) {
	db, err := sql.Open("postgres", databaseURL)
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}

	// Test the connection
	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("failed to ping database: %w", err)
	}

	// Run migrations
	if err := RunMigrations(db); err != nil {
		return nil, fmt.Errorf("failed to run migrations: %w", err)
	}

	// Create queries
	queries := pgdb.New(db)

	return &Database{
		DB:      db,
		Queries: queries,
	}, nil
}
