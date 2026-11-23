package handler

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"golang.org/x/oauth2"

	"github.com/calnode/calnode/internal/dbtest"
)

func TestMicrosoftIdentityEmail(t *testing.T) {
	cases := []struct {
		name      string
		mail, upn string
		verified  []string
		want      string
		wantErr   bool
	}{
		{"mail on the UPN's domain is used", "Jane.Doe@Contoso.com", "jdoe@contoso.com", nil, "jane.doe@contoso.com", false},
		{"no mail falls back to the UPN", "", "jdoe@contoso.com", nil, "jdoe@contoso.com", false},
		// mail is settable by a tenant admin on an account without a mailbox, so an
		// address on a domain the tenant has not verified is not an identity.
		{"mail on another domain is ignored", "ceo@victim.example", "attacker@attacker.onmicrosoft.com", nil, "attacker@attacker.onmicrosoft.com", false},
		{"mail on a domain only another tenant verified is ignored", "ceo@victim.example", "attacker@attacker.onmicrosoft.com",
			[]string{"attacker.onmicrosoft.com", "attacker.example"}, "attacker@attacker.onmicrosoft.com", false},
		// The common member shape: UPN on the tenant's onmicrosoft.com domain, mail on a
		// custom domain the same tenant verified.
		{"mail on one of the tenant's verified domains is used", "Jane@Contoso.com", "jane@contoso.onmicrosoft.com",
			[]string{"contoso.onmicrosoft.com", "Contoso.com"}, "jane@contoso.com", false},
		{"guest UPN is refused", "", "user_victim.example#EXT#@tenant.onmicrosoft.com", nil, "", true},
		{"guest UPN is refused even with a verified mail", "user@victim.example", "user_victim.example#EXT#@tenant.onmicrosoft.com",
			[]string{"victim.example"}, "", true},
		{"mail without a UPN is not trusted", "ceo@victim.example", "", []string{"victim.example"}, "", true},
		{"a mail with no domain falls back to the UPN", "nodomain", "jdoe@contoso.com", []string{""}, "jdoe@contoso.com", false},
		{"nothing at all", "", "", nil, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := microsoftIdentityEmail(microsoftUserInfo{Mail: tc.mail, UserPrincipalName: tc.upn}, tc.verified)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v; wantErr %v", err, tc.wantErr)
			}
			if got != tc.want {
				t.Errorf("got %q; want %q", got, tc.want)
			}
		})
	}
}

func TestEmailDomain(t *testing.T) {
	for in, want := range map[string]string{"a@b.c": "b.c", "a@": "", "nodomain": "", "a@b@c.d": "c.d"} {
		if got := emailDomain(in); got != want {
			t.Errorf("emailDomain(%q) = %q; want %q", in, got, want)
		}
	}
}

// fakeGraph serves /me with the given body and /organization with org (or a status when
// orgStatus is not 200), and counts the /organization reads.
func fakeGraph(t *testing.T, me, org string, orgStatus int) (orgReads *int) {
	t.Helper()
	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/me":
			_, _ = w.Write([]byte(me))
		case "/organization":
			n++
			if orgStatus != http.StatusOK {
				w.WriteHeader(orgStatus)
				return
			}
			_, _ = w.Write([]byte(org))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	prev := microsoftGraphBase
	microsoftGraphBase = srv.URL
	t.Cleanup(func() { microsoftGraphBase = prev })
	return &n
}

// microsoftLogin runs the identity read and the account match the callback runs, against
// the fake Graph, and returns where the login redirected.
func microsoftLogin(t *testing.T, h *Handler) string {
	t.Helper()
	email, err := fetchMicrosoftEmail(context.Background(), &oauth2.Config{}, &oauth2.Token{AccessToken: "t"})
	if err != nil {
		t.Fatalf("fetchMicrosoftEmail: %v", err)
	}
	rec := httptest.NewRecorder()
	h.finishOAuthLogin(rec, httptest.NewRequest(http.MethodGet, "/v1/auth/microsoft/callback", nil), email, "")
	return rec.Header().Get("Location")
}

func microsoftLoginFixture(t *testing.T, accountEmail string) *Handler {
	t.Helper()
	database := dbtest.Open(t)
	t.Cleanup(func() { database.Close() })
	if _, err := database.ExecContext(context.Background(),
		`INSERT INTO users (id, email, name, is_admin) VALUES ('u-ms', ?, 'Jane', 0)`, accountEmail); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	return New(database, slog.Default())
}

// An account created under the previous rule holds the member's mail, which sits on a
// custom domain while the UPN is on onmicrosoft.com. It keeps signing in, because the
// tenant has verified that domain.
func TestMicrosoftLogin_existingAccountOnATenantVerifiedMailDomainKeepsSigningIn(t *testing.T) {
	h := microsoftLoginFixture(t, "jane@contoso.example")
	reads := fakeGraph(t,
		`{"mail":"Jane@Contoso.example","userPrincipalName":"jane@contoso.onmicrosoft.com"}`,
		`{"value":[{"verifiedDomains":[{"name":"contoso.onmicrosoft.com"},{"name":"contoso.example"}]}]}`, http.StatusOK)

	if loc := microsoftLogin(t, h); loc != "/admin" {
		t.Fatalf("redirect %q; want /admin", loc)
	}
	if *reads != 1 {
		t.Errorf("organization reads = %d; want 1", *reads)
	}
}

// A tenant naming, in mail, an address on a domain it has not verified resolves to its own
// UPN, so it reaches no account holding that address.
func TestMicrosoftLogin_mailOnADomainTheTenantDoesNotOwnReachesNoAccount(t *testing.T) {
	h := microsoftLoginFixture(t, "ceo@victim.example")
	fakeGraph(t,
		`{"mail":"ceo@victim.example","userPrincipalName":"admin@attacker.onmicrosoft.com"}`,
		`{"value":[{"verifiedDomains":[{"name":"attacker.onmicrosoft.com"}]}]}`, http.StatusOK)

	if loc := microsoftLogin(t, h); !strings.Contains(loc, "error=no_account") {
		t.Fatalf("redirect %q; want error=no_account", loc)
	}
}

// A user whose account is on their UPN signs in on the UPN, with no organization read
// when mail shares its domain.
func TestMicrosoftLogin_newAccountOnTheUPNDomainSignsInWithoutAnOrganizationRead(t *testing.T) {
	h := microsoftLoginFixture(t, "sam@fabrikam.example")
	reads := fakeGraph(t, `{"mail":"Sam@Fabrikam.example","userPrincipalName":"sam@fabrikam.example"}`, "", http.StatusOK)

	if loc := microsoftLogin(t, h); loc != "/admin" {
		t.Fatalf("redirect %q; want /admin", loc)
	}
	if *reads != 0 {
		t.Errorf("organization reads = %d; want 0", *reads)
	}
}

// An organization read that fails (a personal Microsoft account has no organization)
// falls back to the UPN, never to the unverified mail.
func TestMicrosoftLogin_failedOrganizationReadFallsBackToTheUPN(t *testing.T) {
	h := microsoftLoginFixture(t, "jane@contoso.example")
	fakeGraph(t, `{"mail":"jane@contoso.example","userPrincipalName":"jane@contoso.onmicrosoft.com"}`, "", http.StatusForbidden)

	email, err := fetchMicrosoftEmail(context.Background(), &oauth2.Config{}, &oauth2.Token{AccessToken: "t"})
	if err != nil {
		t.Fatalf("fetchMicrosoftEmail: %v", err)
	}
	if email != "jane@contoso.onmicrosoft.com" {
		t.Errorf("email %q; want the UPN", email)
	}
	if loc := microsoftLogin(t, h); !strings.Contains(loc, "error=no_account") {
		t.Fatalf("redirect %q; want error=no_account", loc)
	}
}

func TestFetchMicrosoftEmail_refusesAFailedMeRead(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/me" && r.URL.Query().Get("bad") == "" {
			_, _ = w.Write([]byte(`{not json`))
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)
	prev := microsoftGraphBase
	t.Cleanup(func() { microsoftGraphBase = prev })

	microsoftGraphBase = srv.URL
	if _, err := fetchMicrosoftEmail(context.Background(), &oauth2.Config{}, &oauth2.Token{AccessToken: "t"}); err == nil {
		t.Error("undecodable /me: want an error")
	}
	microsoftGraphBase = srv.URL + "/nope"
	if _, err := fetchMicrosoftEmail(context.Background(), &oauth2.Config{}, &oauth2.Token{AccessToken: "t"}); err == nil {
		t.Error("401 /me: want an error")
	}
	microsoftGraphBase = "http://127.0.0.1:0"
	if _, err := fetchMicrosoftEmail(context.Background(), &oauth2.Config{}, &oauth2.Token{AccessToken: "t"}); err == nil {
		t.Error("unreachable Graph: want an error")
	}
}
