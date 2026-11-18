package caldav

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// With WithHTTPSOnly, no request goes to a plain-http URL, whichever step of discovery
// produced it: the check is at the one place every request is sent, before the
// credentials are attached and sent.
func TestHTTPSOnlyRefusesPlainHTTPBeforeSending(t *testing.T) {
	var plainHits atomic.Int32
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		plainHits.Add(1)
		w.WriteHeader(http.StatusMultiStatus)
	}))
	defer plain.Close()
	var tlsHits atomic.Int32
	secure := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tlsHits.Add(1)
		w.WriteHeader(http.StatusMultiStatus)
	}))
	defer secure.Close()

	c, err := New(newTestDB(t), testKeyHex, WithHTTPSOnly(true))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c.hc.Transport = secure.Client().Transport
	ctx := context.Background()

	if _, _, _, err := c.do(ctx, "PROPFIND", plain.URL+"/principals/user/", "user", "app-pw", "0", ""); err == nil {
		t.Error("request to an http URL succeeded; want it refused")
	}
	if n := plainHits.Load(); n != 0 {
		t.Errorf("the http server received %d request(s); want 0", n)
	}
	if status, _, _, err := c.do(ctx, "PROPFIND", secure.URL+"/principals/user/", "user", "app-pw", "0", ""); err != nil || status != http.StatusMultiStatus {
		t.Errorf("https request: status %d, err %v; want 207", status, err)
	}
	if tlsHits.Load() != 1 {
		t.Errorf("https server hits = %d; want 1", tlsHits.Load())
	}

	// Without the option a single-tenant instance keeps plain http.
	open, err := New(newTestDB(t), testKeyHex)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	open.hc.Transport = nil
	if _, _, _, err := open.do(ctx, "PROPFIND", plain.URL+"/", "user", "app-pw", "0", ""); err != nil {
		t.Errorf("single-tenant http request: %v", err)
	}
}
