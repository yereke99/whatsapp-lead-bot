// Command migrate applies the database schema and exits. Useful as a
// deployment step when DATABASE_AUTO_MIGRATE is turned off.
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/ayran/whatsapp-automation/internal/config"
	"github.com/ayran/whatsapp-automation/internal/logging"
	"github.com/ayran/whatsapp-automation/internal/storage/sqlite"
	"github.com/ayran/whatsapp-automation/migrations"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "fatal: %v\n", err)
		os.Exit(1)
	}

	log := logging.New(cfg.App.LogLevel, cfg.App.LogFormat)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	db, err := sqlite.Connect(ctx, cfg.Database)
	if err != nil {
		log.Error("connect to database failed", "error", err)
		os.Exit(1)
	}
	defer db.Close()

	if err := db.Migrate(ctx, migrations.FS, log); err != nil {
		log.Error("migration failed", "error", err)
		os.Exit(1)
	}

	log.Info("migrations applied")
}
