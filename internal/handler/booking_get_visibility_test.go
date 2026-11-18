package handler_test

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// GET /v1/bookings/{id} applies the same visibility rule as the list: a member sees a
// booking only when they host it (primary or assigned), an admin sees any.
func TestGetBooking_memberSeesOnlyBookingsTheyHost(t *testing.T) {
	h, database, ownerKey, ownerID := setupWorkspaceWithDB(t)
	_, etID := seedEventTypeHTTP(t, h, ownerKey)
	seedRoleUser(t, database, "m1", "m1@example.com", 0, 0, "member-1-key")
	seedRoleUser(t, database, "m2", "m2@example.com", 0, 0, "member-2-key")

	seedBooking(t, database, "b-owner", etID, ownerID, "2027-07-01T10:00:00Z", "2027-07-01T10:30:00Z", "confirmed")
	seedBooking(t, database, "b-m1", etID, "m1", "2027-07-02T10:00:00Z", "2027-07-02T10:30:00Z", "confirmed")
	seedBooking(t, database, "b-seat", etID, ownerID, "2027-07-03T10:00:00Z", "2027-07-03T10:30:00Z", "confirmed")
	if _, err := database.Exec(`INSERT INTO booking_hosts (id, booking_id, user_id) VALUES ('bh-seat', 'b-seat', 'm2')`); err != nil {
		t.Fatalf("seed seat: %v", err)
	}

	get := func(key, id string) int {
		req := authReq(http.MethodGet, "/v1/bookings/"+id, "", key)
		req.SetPathValue("id", id)
		rec := httptest.NewRecorder()
		h.RequireAuth(h.GetBooking)(rec, req)
		return rec.Code
	}
	for _, tc := range []struct {
		who, key, id string
		want         int
	}{
		{"owner (admin) reads another host's booking", ownerKey, "b-m1", http.StatusOK},
		{"member reads their own booking", "member-1-key", "b-m1", http.StatusOK},
		{"member reads a booking they hold a seat on", "member-2-key", "b-seat", http.StatusOK},
		{"member cannot read another host's booking", "member-1-key", "b-owner", http.StatusNotFound},
		{"member cannot read a booking they have no seat on", "member-2-key", "b-m1", http.StatusNotFound},
	} {
		if got := get(tc.key, tc.id); got != tc.want {
			t.Errorf("%s: status = %d; want %d", tc.who, got, tc.want)
		}
	}
}
