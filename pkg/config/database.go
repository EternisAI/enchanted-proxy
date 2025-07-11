package config

import (
	"github.com/eternisai/enchanted-proxy/pkg/storage/pg"
)

// InitDatabase initializes the database connection and runs migrations.
func InitDatabase() (*pg.Database, error) {
	return pg.InitDatabase(AppConfig.DatabaseURL)
}
