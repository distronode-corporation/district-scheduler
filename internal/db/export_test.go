package db

import (
	"fmt"

	"github.com/pressly/goose/v3"
)

// MigrateAllowingMissing is Migrate with goose's allow-missing option, for tests that
// re-run one data migration by forgetting its goose_db_version row. Plain Migrate
// refuses that once any later migration is applied ("found 1 missing migrations before
// current version"), which is the right answer at boot and the wrong one for a replay.
// Same embedded FS, same dialect selection, same lock as the boot path.
func MigrateAllowingMissing(h *DB) error {
	gooseMu.Lock()
	defer gooseMu.Unlock()

	goose.SetBaseFS(migrations)
	if err := goose.SetDialect(h.dialect.gooseDialect()); err != nil {
		return fmt.Errorf("set goose dialect: %w", err)
	}
	if err := goose.Up(h.DB, h.dialect.migrationsDir(), goose.WithAllowMissing()); err != nil {
		return fmt.Errorf("run migrations: %w", err)
	}
	return nil
}
