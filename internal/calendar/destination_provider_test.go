package calendar

import (
	"context"
	"testing"

	"github.com/calnode/calnode/internal/dbtest"
)

func TestConnectedPrefersDestinationOverOlderCalDAV(t *testing.T) {
	database := dbtest.Open(t)
	if _, err := database.Exec(`INSERT INTO users (id, email, name, iana_timezone, is_admin, created_at)
		VALUES ('host', 'host@example.com', 'Host', 'Europe/Amsterdam', 0, '2026-09-20T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	for _, provider := range []string{"caldav", "google"} {
		destination := 0
		if provider == "google" {
			destination = 1
		}
		if _, err := database.Exec(`INSERT INTO calendar_connections
			(id, user_id, provider, access_token_enc, calendar_id, check_conflicts, is_destination, created_at)
			VALUES (?, 'host', ?, 'encrypted', 'primary', 1, ?, '2026-09-20T00:00:00Z')`, provider, provider, destination); err != nil {
			t.Fatal(err)
		}
	}
	svc := NewService(database)
	connected, provider, err := svc.Connected(context.Background(), "host")
	if err != nil || !connected || provider != "google" {
		t.Fatalf("connected=%v provider=%q error=%v; want Google destination", connected, provider, err)
	}
	if err := svc.SetDestination(context.Background(), "host", "caldav", ""); err != nil {
		t.Fatal(err)
	}
	_, provider, err = svc.Connected(context.Background(), "host")
	if err != nil || provider != "caldav" {
		t.Fatalf("provider=%q error=%v; want changed destination", provider, err)
	}
}
