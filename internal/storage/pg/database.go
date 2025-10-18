package pg

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/eternisai/enchanted-proxy/internal/config"
	pgdb "github.com/eternisai/enchanted-proxy/internal/storage/pg/sqlc"
	tasksdb "github.com/eternisai/enchanted-proxy/internal/storage/pg/queries/tasks"
	_ "github.com/lib/pq"
)

type Database struct {
	DB           *sql.DB
	Queries      *pgdb.Queries
	TasksQueries *tasksdb.Queries
}

// InitDatabase initializes the database connection and runs migrations.
func InitDatabase(databaseURL string) (*Database, error) {
	db, err := sql.Open("postgres", databaseURL)
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}

	db.SetMaxOpenConns(config.AppConfig.DBMaxOpenConns)
	db.SetMaxIdleConns(config.AppConfig.DBMaxIdleConns)
	db.SetConnMaxIdleTime(time.Duration(config.AppConfig.DBConnMaxIdleTime) * time.Minute)
	db.SetConnMaxLifetime(time.Duration(config.AppConfig.DBConnMaxLifetime) * time.Minute)

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
	tasksQueries := tasksdb.New(db)

	return &Database{
		DB:           db,
		Queries:      queries,
		TasksQueries: tasksQueries,
	}, nil
}
