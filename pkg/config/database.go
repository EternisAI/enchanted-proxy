package config

import (
	"github.com/eternisai/enchanted-proxy/pkg/invitecode"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

// InitDatabase initializes the database connection and runs migrations.
func InitDatabase() (*gorm.DB, error) {
	db, err := gorm.Open(postgres.Open(AppConfig.DatabaseURL), &gorm.Config{})
	if err != nil {
		return nil, err
	}

	// Auto migrate the models
	err = db.AutoMigrate(&invitecode.InviteCode{})
	if err != nil {
		return nil, err
	}

	return db, nil
}
