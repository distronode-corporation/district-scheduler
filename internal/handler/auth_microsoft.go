package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/microsoft"
)

// SetMicrosoftAuth configures Microsoft (Entra) OAuth sign-in. Identity only —
// openid/email/profile/User.Read; calendar access is a separate connection with its
// own scopes. Called from server.New when Microsoft credentials are configured.
// secure should be true when BASE_URL starts with https://.
func (h *Handler) SetMicrosoftAuth(clientID, clientSecret, tenant, redirectURL string, secure bool) {
	if tenant == "" {
		tenant = "common"
	}
	cfg := &oauth2.Config{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		Endpoint:     microsoft.AzureADEndpoint(tenant),
		RedirectURL:  redirectURL,
		Scopes:       []string{"openid", "email", "profile", "User.Read"},
	}
	h.authMu.Lock()
	h.microsoftAuth = cfg
	h.secureCookie = secure
	h.authMu.Unlock()
}

// LoginMicrosoft redirects to Microsoft's OAuth consent screen.
// GET /v1/auth/microsoft/login
func (h *Handler) LoginMicrosoft(w http.ResponseWriter, r *http.Request) {
	ma := h.getMicrosoftAuth()
	if ma == nil {
		http.Error(w, "Microsoft OAuth not configured", http.StatusServiceUnavailable)
		return
	}
	// ⛔ The workspace is resolved HERE, from the Host the person clicked "sign in" on. The
	// callback arrives on the identity host and cannot resolve it, which is why it rides the
	// state cookie. /v1/auth/login is Platform-wrapped (it has to be, because its callback
	// is), so this does explicitly what HostWorkspace would have done — including 404 on a
	// host that names no workspace, rather than defaulting to a tenant nobody chose.
	loginWorkspace := ""
	if h.multiTenant {
		ws, wsErr := h.workspaceByHost(r.Context(), r.Host)
		if wsErr != nil {
			h.writeResolveError(w, r, wsErr)
			return
		}
		loginWorkspace = ws.ID
	}
	state, err := h.newOAuthState(w, loginWorkspace)
	if err != nil {
		h.logger.ErrorContext(r.Context(), "auth: generate state", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	// prompt=select_account so a cached SSO session can't silently pick the wrong one.
	http.Redirect(w, r, ma.AuthCodeURL(state, oauth2.AccessTypeOnline,
		oauth2.SetAuthURLParam("prompt", "select_account")), http.StatusFound)
}

// CallbackMicrosoft handles the OAuth redirect from Microsoft.
// GET /v1/auth/microsoft/callback
func (h *Handler) CallbackMicrosoft(w http.ResponseWriter, r *http.Request) {
	ma := h.getMicrosoftAuth()
	if ma == nil {
		http.Error(w, "Microsoft OAuth not configured", http.StatusServiceUnavailable)
		return
	}
	stateWorkspace, stateOK := h.verifyOAuthState(w, r)
	if !stateOK {
		http.Redirect(w, r, "/admin/login?error=state", http.StatusFound)
		return
	}
	if r.URL.Query().Get("error") != "" {
		http.Redirect(w, r, "/admin/login?error=denied", http.StatusFound)
		return
	}

	tok, err := ma.Exchange(r.Context(), r.URL.Query().Get("code"))
	if err != nil {
		h.logger.ErrorContext(r.Context(), "auth: microsoft token exchange", "error", err)
		http.Redirect(w, r, "/admin/login?error=oauth", http.StatusFound)
		return
	}

	email, err := fetchMicrosoftEmail(r.Context(), ma, tok)
	if err != nil {
		h.logger.ErrorContext(r.Context(), "auth: microsoft user info", "error", err)
		http.Redirect(w, r, "/admin/login?error=userinfo", http.StatusFound)
		return
	}
	h.finishOAuthLogin(w, r, email, stateWorkspace)
}

type microsoftUserInfo struct {
	Mail              string `json:"mail"`
	UserPrincipalName string `json:"userPrincipalName"`
}

// microsoftGraphBase is the Graph root the sign-in reads. A variable so tests can point it
// at a local server; nothing else assigns it.
var microsoftGraphBase = "https://graph.microsoft.com/v1.0"

// fetchMicrosoftEmail reads the signed-in account's email from Graph /me. Lowercased to
// match stored account emails.
//
// Graph /me has no "verified" boolean to check (unlike Google's userinfo, see
// auth_google.go), so the two fields are trusted differently:
//
//   - userPrincipalName must carry one of the signing-in tenant's verified domains (or its
//     onmicrosoft.com domain), which Entra enforces. The one UPN shape that is NOT an owned
//     address is a B2B guest's, synthesized as "user_domain.com#EXT#@resourcetenant...",
//     and it is refused.
//   - mail is NOT domain-verified: a tenant administrator can set it to any address on an
//     account without an Exchange mailbox. It is therefore used only when its domain is one
//     the signing-in tenant has proved it owns: the UPN's domain, or any entry of the
//     tenant's verifiedDomains (Graph /organization, readable with User.Read). A domain is
//     verified in at most one tenant, so another tenant can never name an address it does
//     not own and be signed in as the Calnode user who holds it. Otherwise the UPN is the
//     identity.
//
// The verifiedDomains read is what keeps existing accounts signing in. Accounts are matched
// by email alone (finishOAuthLogin), and the previous rule preferred mail unconditionally,
// so an account created with a member's mail on a custom domain (UPN still on
// onmicrosoft.com, or on a second domain of the same tenant) holds that mail. It is only
// made when the two domains differ, and a failed read (a personal Microsoft account, which
// has no organization) falls back to the UPN, never to the unverified mail.
func fetchMicrosoftEmail(ctx context.Context, cfg *oauth2.Config, tok *oauth2.Token) (string, error) {
	client := cfg.Client(ctx, tok)
	var info microsoftUserInfo
	if err := getGraphJSON(client, microsoftGraphBase+"/me?$select=mail,userPrincipalName", &info); err != nil {
		return "", fmt.Errorf("auth: graph /me: %w", err)
	}
	var verified []string
	mail := strings.ToLower(strings.TrimSpace(info.Mail))
	upn := strings.ToLower(strings.TrimSpace(info.UserPrincipalName))
	if mail != "" && emailDomain(mail) != emailDomain(upn) {
		var org struct {
			Value []struct {
				VerifiedDomains []struct {
					Name string `json:"name"`
				} `json:"verifiedDomains"`
			} `json:"value"`
		}
		if err := getGraphJSON(client, microsoftGraphBase+"/organization?$select=verifiedDomains", &org); err == nil {
			for _, o := range org.Value {
				for _, d := range o.VerifiedDomains {
					verified = append(verified, d.Name)
				}
			}
		}
	}
	return microsoftIdentityEmail(info, verified)
}

// getGraphJSON GETs url and decodes a 200 response into out.
func getGraphJSON(client *http.Client, url string, out any) error {
	resp, err := client.Get(url) // #nosec G107 -- url is microsoftGraphBase plus a constant path
	if err != nil {
		return fmt.Errorf("request: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck // read-only body
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decode: %w", err)
	}
	return nil
}

// microsoftIdentityEmail applies the trust rule documented on fetchMicrosoftEmail.
// verifiedDomains are the signing-in tenant's verified domain names.
func microsoftIdentityEmail(info microsoftUserInfo, verifiedDomains []string) (string, error) {
	upn := strings.ToLower(strings.TrimSpace(info.UserPrincipalName))
	if strings.Contains(upn, "#ext#") {
		return "", fmt.Errorf("auth: Microsoft account is a B2B guest identity, not a verifiable email")
	}
	if upn == "" {
		return "", fmt.Errorf("auth: no verifiable email from Microsoft")
	}
	mail := strings.ToLower(strings.TrimSpace(info.Mail))
	if md := emailDomain(mail); md != "" {
		if md == emailDomain(upn) {
			return mail, nil
		}
		for _, d := range verifiedDomains {
			if strings.ToLower(strings.TrimSpace(d)) == md {
				return mail, nil
			}
		}
	}
	return upn, nil
}

// emailDomain returns the part after the last "@", or "" when there is none.
func emailDomain(addr string) string {
	i := strings.LastIndex(addr, "@")
	if i < 0 || i == len(addr)-1 {
		return ""
	}
	return addr[i+1:]
}
