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
//     account without an Exchange mailbox. It is therefore used only when its domain is
//     the UPN's domain, i.e. a domain this tenant has proved it owns. Otherwise the UPN is
//     the identity, so another tenant can never name an address it does not own and be
//     signed in as the Calnode user who holds it.
func fetchMicrosoftEmail(ctx context.Context, cfg *oauth2.Config, tok *oauth2.Token) (string, error) {
	resp, err := cfg.Client(ctx, tok).Get("https://graph.microsoft.com/v1.0/me?$select=mail,userPrincipalName")
	if err != nil {
		return "", fmt.Errorf("auth: graph /me request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("auth: graph /me status %d", resp.StatusCode)
	}
	var info microsoftUserInfo
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return "", fmt.Errorf("auth: decode graph /me: %w", err)
	}
	return microsoftIdentityEmail(info)
}

// microsoftIdentityEmail applies the trust rule documented on fetchMicrosoftEmail.
func microsoftIdentityEmail(info microsoftUserInfo) (string, error) {
	upn := strings.ToLower(strings.TrimSpace(info.UserPrincipalName))
	if strings.Contains(upn, "#ext#") {
		return "", fmt.Errorf("auth: Microsoft account is a B2B guest identity, not a verifiable email")
	}
	mail := strings.ToLower(strings.TrimSpace(info.Mail))
	if mail != "" && upn != "" && emailDomain(mail) != "" && emailDomain(mail) == emailDomain(upn) {
		return mail, nil
	}
	if upn == "" {
		return "", fmt.Errorf("auth: no verifiable email from Microsoft")
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
