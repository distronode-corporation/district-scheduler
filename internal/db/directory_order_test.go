package db_test

import (
	"testing"

	"github.com/calnode/calnode/internal/db"
)

// Upstream's display-order migration (its 00070, this fork's 00078) must leave an existing
// event type exactly as it was, at order 0. SQLite only, as upstream wrote it: dbtest hands
// back a database that is already at the target version, and an additive column cannot be
// replayed, so the test needs a fresh handle stopped one migration short.
func TestMigrate_displayOrderPreservesExistingEvents(t *testing.T) {
	handle, err := db.OpenDB("sqlite://:memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { handle.Close() }) // #nosec G104 -- in-memory test handle
	// Start from the schema immediately before display_order was introduced.
	if err := db.MigrateUpTo(handle, 77); err != nil {
		t.Fatal(err)
	}
	if _, err := handle.Exec(`INSERT INTO users (id,email,name) VALUES ('owner','owner@example.com','Owner')`); err != nil {
		t.Fatal(err)
	}
	if _, err := handle.Exec(`INSERT INTO event_types (id,user_id,slug,name,duration_minutes) VALUES ('event','owner','existing','Existing event',30)`); err != nil {
		t.Fatal(err)
	}
	if err := handle.Migrate(); err != nil {
		t.Fatal(err)
	}
	var name, slug string
	var order int
	if err := handle.QueryRow(`SELECT name,slug,display_order FROM event_types WHERE id = 'event'`).Scan(&name, &slug, &order); err != nil {
		t.Fatal(err)
	}
	if name != "Existing event" || slug != "existing" || order != 0 {
		t.Fatalf("existing event changed: name=%q slug=%q order=%d", name, slug, order)
	}
}
