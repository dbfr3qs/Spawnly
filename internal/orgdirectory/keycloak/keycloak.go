// Package keycloak implements orgdirectory.Directory against Keycloak's admin
// REST API (Organizations + Users). It authenticates as a dedicated confidential
// service-account client (org-admin) via client_credentials, holding only the
// minimal realm-management roles (manage-users, manage-realm) — never the
// bootstrap/master admin. The client secret arrives at runtime from the
// bootstrap-generated Secret (deploy/keycloak + scripts/bootstrap.sh); it is
// never baked into an image or committed.
//
// All org member operations address Keycloak by the org's UUID and users by
// their UUID, but the platform's identifiers are the org ALIAS (== OrgID) and the
// username (== Spawnly userId). This adapter resolves alias->UUID and
// username->UUID internally so callers only ever deal in the platform ids.
package keycloak

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/spawnly/platform/internal/orgdirectory"
)

// Client is the Keycloak-backed orgdirectory.Directory.
type Client struct {
	base              string // Keycloak base URL, e.g. http://keycloak:8080 (no trailing slash)
	realm             string // realm name, e.g. spawnly
	clientID          string // service-account client id, e.g. org-admin
	clientSecret      string
	dashboardClientID string // OIDC client whose redirect URIs the satellite feature edits, e.g. dashboard

	hc *http.Client

	mu          sync.Mutex // guards the cached token AND the cached dashboard client UUID
	cachedToken string
	tokenExp    time.Time

	dashboardUUID string // resolved dashboard client UUID, cached (see resolveDashboardClientID)

	// editMu serializes the read-modify-write of the dashboard client rep
	// (resolve -> GET -> PUT). Keycloak's client PUT has no optimistic locking, so
	// two concurrent org creates could otherwise each GET the same rep, add their
	// own URI, and PUT-clobber each other (a lost update — one org's satellite
	// login silently breaks). The registry runs single-replica, so a process-local
	// mutex is sufficient; a multi-replica registry would need a distributed lock.
	editMu sync.Mutex
}

var _ orgdirectory.Directory = (*Client)(nil)

// New builds a Keycloak directory client. httpClient may be nil (a 15s-timeout
// default is used) — a bounded timeout matters here because these calls sit
// inline on the org control-plane path. dashboardClientID is the OIDC client
// (e.g. "dashboard") whose redirect/post-logout URIs the satellite feature edits.
func New(base, realm, clientID, clientSecret, dashboardClientID string, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 15 * time.Second}
	}
	return &Client{
		base:              strings.TrimRight(base, "/"),
		realm:             realm,
		clientID:          clientID,
		clientSecret:      clientSecret,
		dashboardClientID: dashboardClientID,
		hc:                httpClient,
	}
}

func (c *Client) adminURL(format string, args ...any) string {
	return c.base + "/admin/realms/" + c.realm + fmt.Sprintf(format, args...)
}

// token returns a cached service-account access token, minting a fresh one via
// client_credentials when the cache is empty or near expiry.
func (c *Client) token(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	// 30s skew guard so an about-to-expire token isn't handed out.
	if c.cachedToken != "" && time.Now().Add(30*time.Second).Before(c.tokenExp) {
		return c.cachedToken, nil
	}
	form := url.Values{
		"grant_type":    {"client_credentials"},
		"client_id":     {c.clientID},
		"client_secret": {c.clientSecret},
	}
	tokenURL := c.base + "/realms/" + c.realm + "/protocol/openid-connect/token"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("org-admin token request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// Do NOT echo the body — it can carry client-secret-adjacent detail.
		return "", fmt.Errorf("org-admin token request: status %d", resp.StatusCode)
	}
	var tr struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tr); err != nil {
		return "", fmt.Errorf("org-admin token decode: %w", err)
	}
	if tr.AccessToken == "" {
		return "", fmt.Errorf("org-admin token: empty access_token")
	}
	c.cachedToken = tr.AccessToken
	c.tokenExp = time.Now().Add(time.Duration(tr.ExpiresIn) * time.Second)
	return c.cachedToken, nil
}

// do issues an authenticated admin-REST request. body (if non-nil) is
// JSON-encoded. On a 2xx it decodes into out when out is non-nil. It returns an
// apiError carrying the status on any non-2xx so callers can special-case 404/409
// for idempotency without leaking the body.
func (c *Client) do(ctx context.Context, method, url string, body, out any) error {
	tok, err := c.token(ctx)
	if err != nil {
		return err
	}
	var rdr *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &apiError{status: resp.StatusCode, method: method}
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

// apiError is a non-2xx admin-REST response. It carries only the status code (not
// the body) so nothing sensitive leaks up the call stack.
type apiError struct {
	status int
	method string
}

func (e *apiError) Error() string {
	return fmt.Sprintf("keycloak admin %s: status %d", e.method, e.status)
}

func statusIs(err error, code int) bool {
	var ae *apiError
	if e, ok := err.(*apiError); ok {
		ae = e
	}
	return ae != nil && ae.status == code
}

// ---- resolution helpers ---------------------------------------------------

type orgRep struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Alias string `json:"alias"`
}

// resolveOrgID maps an org alias to its Keycloak UUID. Keycloak's org GET/DELETE
// paths take the UUID, not the alias, so every member/delete op resolves first.
// Returns ("", nil) when no org has that alias.
func (c *Client) resolveOrgID(ctx context.Context, alias string) (string, error) {
	var orgs []orgRep
	// Query by the ALIAS attribute (q=alias:<alias>) — the ?search= param matches
	// only NAME/domains, so it misses orgs whose name differs from their alias
	// (verified against a live Keycloak 26.4). We still exact-match client-side.
	if err := c.do(ctx, http.MethodGet, c.adminURL("/organizations?q=%s", url.QueryEscape("alias:"+alias)), nil, &orgs); err != nil {
		return "", err
	}
	for _, o := range orgs {
		if o.Alias == alias {
			return o.ID, nil
		}
	}
	return "", nil
}

type userRep struct {
	ID       string              `json:"id"`
	Username string              `json:"username"`
	Email    string              `json:"email"`
	Attrs    map[string][]string `json:"attributes,omitempty"`
}

// resolveUserID maps a username to its Keycloak UUID. Returns ("", false, nil)
// when no such user exists.
func (c *Client) resolveUserID(ctx context.Context, username string) (string, bool, error) {
	var users []userRep
	if err := c.do(ctx, http.MethodGet, c.adminURL("/users?username=%s&exact=true", url.QueryEscape(username)), nil, &users); err != nil {
		return "", false, err
	}
	for _, u := range users {
		if u.Username == username {
			return u.ID, true, nil
		}
	}
	return "", false, nil
}

// createUser creates a disabled-password user who must set a password on first
// login (UPDATE_PASSWORD required action). username is the Spawnly userId; when
// it looks like an email it is also set as the email. Returns the new UUID.
//
// emailVerified is pinned true so directory-provisioned users bypass the realm's
// email-verification requirement (signup-lifecycle Phase 1): verification is a
// self-serve-registration control only. These accounts are created through the
// AddMember path — i.e. an ORG ADMIN adding a member by userId/email (note: a
// self-serve signup becomes admin of their own org, so "org admin" is broader
// than "master admin") or master-admin user provisioning — not by a prospective
// user proving control of an inbox. The account is password-disabled with an
// UPDATE_PASSWORD required action, so no credential is granted here and any reset
// mail still goes to the real inbox; only VERIFY_EMAIL is withheld. Follow-up
// (signup GA hardening, tracked in the plan): once SMTP is universal, consider
// leaving org-admin-invited members UNVERIFIED so the genuine invitee proves the
// inbox at first login, and pinning verified only on the master-admin path — so
// the email_verified claim stays a trustworthy signal for arbitrary addresses.
func (c *Client) createUser(ctx context.Context, username string) (string, error) {
	body := map[string]any{
		"username":        username,
		"enabled":         true,
		"emailVerified":   true,
		"requiredActions": []string{"UPDATE_PASSWORD"},
	}
	if strings.Contains(username, "@") {
		body["email"] = username
	}
	// Keycloak returns 201 with the new id in the Location header and no body.
	tok, err := c.token(ctx)
	if err != nil {
		return "", err
	}
	b, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.adminURL("/users"), bytes.NewReader(b))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.hc.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	// 201 = created (Location has the id). 409 = the user already exists — a
	// concurrent AddMember, or a case-normalized username our exact-match
	// resolveUserID missed; either way fall through to the readback rather than
	// failing, so AddMember stays idempotent.
	if resp.StatusCode == http.StatusCreated {
		if id := idFromLocation(resp.Header.Get("Location")); id != "" {
			return id, nil
		}
	} else if resp.StatusCode != http.StatusConflict {
		return "", &apiError{status: resp.StatusCode, method: "POST /users"}
	}
	// Readback: Location was dropped (some proxies) or the create 409'd.
	id, found, err := c.resolveUserID(ctx, username)
	if err != nil {
		return "", err
	}
	if !found {
		return "", fmt.Errorf("created user %q not found on readback", username)
	}
	return id, nil
}

func idFromLocation(loc string) string {
	if loc == "" {
		return ""
	}
	return loc[strings.LastIndex(loc, "/")+1:]
}

// ---- Directory implementation ---------------------------------------------

func (c *Client) CreateOrg(ctx context.Context, orgID, name string) error {
	// Idempotent: if an org with this alias already exists, succeed.
	existing, err := c.resolveOrgID(ctx, orgID)
	if err != nil {
		return err
	}
	if existing != "" {
		return nil
	}
	if name == "" {
		name = orgID
	}
	body := map[string]any{"name": name, "alias": orgID}
	if err := c.do(ctx, http.MethodPost, c.adminURL("/organizations"), body, nil); err != nil {
		// A racing create (409) means the org now exists — converge to success.
		if statusIs(err, http.StatusConflict) {
			return nil
		}
		return err
	}
	return nil
}

func (c *Client) DeleteOrg(ctx context.Context, orgID string) error {
	id, err := c.resolveOrgID(ctx, orgID)
	if err != nil {
		return err
	}
	if id == "" {
		return nil // already gone
	}
	if err := c.do(ctx, http.MethodDelete, c.adminURL("/organizations/%s", id), nil, nil); err != nil {
		if statusIs(err, http.StatusNotFound) {
			return nil
		}
		return err
	}
	return nil
}

func (c *Client) AddMember(ctx context.Context, orgID, user string) error {
	orgUUID, err := c.resolveOrgID(ctx, orgID)
	if err != nil {
		return err
	}
	if orgUUID == "" {
		return fmt.Errorf("org %q not found", orgID)
	}
	userUUID, found, err := c.resolveUserID(ctx, user)
	if err != nil {
		return err
	}
	if !found {
		userUUID, err = c.createUser(ctx, user)
		if err != nil {
			return err
		}
	}
	// Members endpoint takes the user id as a RAW JSON STRING body.
	if err := c.do(ctx, http.MethodPost, c.adminURL("/organizations/%s/members", orgUUID), userUUID, nil); err != nil {
		// 409 = already a member; converge to success.
		if statusIs(err, http.StatusConflict) {
			return nil
		}
		return err
	}
	return nil
}

func (c *Client) RemoveMember(ctx context.Context, orgID, user string) error {
	orgUUID, err := c.resolveOrgID(ctx, orgID)
	if err != nil {
		return err
	}
	if orgUUID == "" {
		return nil // no org => nothing to remove
	}
	userUUID, found, err := c.resolveUserID(ctx, user)
	if err != nil {
		return err
	}
	if !found {
		return nil // unknown user => not a member
	}
	if err := c.do(ctx, http.MethodDelete, c.adminURL("/organizations/%s/members/%s", orgUUID, userUUID), nil, nil); err != nil {
		if statusIs(err, http.StatusNotFound) {
			return nil
		}
		return err
	}
	return nil
}

func (c *Client) SetRole(ctx context.Context, orgID, user, role string) error {
	if !orgdirectory.ValidRole(role) {
		return fmt.Errorf("invalid role %q", role)
	}
	// Role is not a Keycloak concept (see the package doc): the authoritative
	// member|admin tuple lives in SpiceDB and is written by the caller. Here we
	// only assert the subject exists so a typo'd username fails loudly instead of
	// silently succeeding, keeping the dual-write honest.
	_, found, err := c.resolveUserID(ctx, user)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("user %q not found", user)
	}
	return nil
}

// pageSize is the admin-REST page window we walk lists in. Keycloak caps a page
// (default 100), so a single unpaginated GET would silently truncate a large
// org's members — and a truncated member list would leave tuples uncleaned on
// org delete. We page until a short page.
const pageSize = 100

func (c *Client) ListMembers(ctx context.Context, orgID string) ([]orgdirectory.Member, error) {
	orgUUID, err := c.resolveOrgID(ctx, orgID)
	if err != nil {
		return nil, err
	}
	if orgUUID == "" {
		return nil, fmt.Errorf("org %q not found", orgID)
	}
	var members []orgdirectory.Member
	for first := 0; ; first += pageSize {
		var page []userRep
		if err := c.do(ctx, http.MethodGet,
			c.adminURL("/organizations/%s/members?first=%d&max=%d", orgUUID, first, pageSize), nil, &page); err != nil {
			return nil, err
		}
		for _, u := range page {
			members = append(members, orgdirectory.Member{
				UserID:   u.Username, // Spawnly userId == username == SpiceDB subject id
				Username: u.Username,
				Email:    u.Email,
				// Role deliberately empty — resolved from SpiceDB by the UI (Phase 6).
			})
		}
		if len(page) < pageSize {
			break
		}
	}
	return members, nil
}

func (c *Client) ListUserOrgs(ctx context.Context, user string) ([]string, error) {
	userUUID, found, err := c.resolveUserID(ctx, user)
	if err != nil {
		return nil, err
	}
	if !found {
		return []string{}, nil // unknown user is in no orgs (no existence oracle)
	}
	var aliases []string
	for first := 0; ; first += pageSize {
		var page []orgRep
		if err := c.do(ctx, http.MethodGet,
			c.adminURL("/organizations/members/%s/organizations?first=%d&max=%d", userUUID, first, pageSize), nil, &page); err != nil {
			return nil, err
		}
		for _, o := range page {
			aliases = append(aliases, o.Alias)
		}
		if len(page) < pageSize {
			break
		}
	}
	return aliases, nil
}

// ---- satellite redirect-URI editing (org-subdomains) ----------------------

// postLogoutSep is how Keycloak serializes multiple post-logout redirect URIs
// into the single `post.logout.redirect.uris` client attribute string — the same
// `##` join deploy/keycloak/clients.tf (valid_post_logout_redirect_uris) uses.
const postLogoutSep = "##"

// clientRep is a partial view of a Keycloak client representation. The full
// representation is round-tripped as a map (see editDashboardClient) so unknown
// fields survive the PUT; this typed view is only used to resolve the client id.
type clientRep struct {
	ID       string `json:"id"`
	ClientID string `json:"clientId"`
}

// resolveDashboardClientID resolves and caches the dashboard OIDC client's UUID.
// The client's redirect-URI edits address it by UUID (like org ops address orgs
// by UUID). A missing client is a hard error — the feature cannot register a
// satellite callback without it. Cached under c.mu like the token.
func (c *Client) resolveDashboardClientID(ctx context.Context) (string, error) {
	c.mu.Lock()
	if c.dashboardUUID != "" {
		id := c.dashboardUUID
		c.mu.Unlock()
		return id, nil
	}
	c.mu.Unlock()

	var reps []clientRep
	if err := c.do(ctx, http.MethodGet,
		c.adminURL("/clients?clientId=%s", url.QueryEscape(c.dashboardClientID)), nil, &reps); err != nil {
		return "", err
	}
	for _, r := range reps {
		if r.ClientID == c.dashboardClientID {
			c.mu.Lock()
			c.dashboardUUID = r.ID
			c.mu.Unlock()
			return r.ID, nil
		}
	}
	return "", fmt.Errorf("dashboard client %q not found", c.dashboardClientID)
}

// editDashboardClient reads the FULL dashboard client representation, applies mut
// to it (mut mutates redirectUris / attributes in place), and PUTs the whole rep
// back. Keycloak's PUT REPLACES the representation, so the full map is preserved
// and only the fields mut touches change. mut reports whether anything changed;
// when nothing changed the PUT is skipped (idempotent no-op).
func (c *Client) editDashboardClient(ctx context.Context, mut func(rep map[string]any) bool) error {
	c.editMu.Lock()
	defer c.editMu.Unlock()

	uuid, err := c.resolveDashboardClientID(ctx)
	if err != nil {
		return err
	}
	var rep map[string]any
	if err := c.do(ctx, http.MethodGet, c.adminURL("/clients/%s", uuid), nil, &rep); err != nil {
		// A 404 means the cached UUID is stale (the dashboard client was recreated —
		// e.g. a realm wipe/re-apply while the registry stayed up). Drop the cache so
		// the caller's retry re-resolves the fresh UUID rather than 404ing forever.
		if statusIs(err, http.StatusNotFound) {
			c.invalidateDashboardCache(uuid)
		}
		return err
	}
	if !mut(rep) {
		return nil // already in the desired state — idempotent
	}
	if err := c.do(ctx, http.MethodPut, c.adminURL("/clients/%s", uuid), rep, nil); err != nil {
		if statusIs(err, http.StatusNotFound) {
			c.invalidateDashboardCache(uuid)
		}
		return err
	}
	return nil
}

// invalidateDashboardCache clears the cached dashboard UUID iff it still equals
// stale (so a concurrent re-resolve to a fresh UUID isn't wiped).
func (c *Client) invalidateDashboardCache(stale string) {
	c.mu.Lock()
	if c.dashboardUUID == stale {
		c.dashboardUUID = ""
	}
	c.mu.Unlock()
}

// asStringSlice reads a JSON array field (unmarshaled to []any of strings).
func asStringSlice(v any) []string {
	arr, _ := v.([]any)
	out := make([]string, 0, len(arr))
	for _, e := range arr {
		if s, ok := e.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// clientAttrs returns the client's attributes map (values are strings), creating
// it on the rep when absent so callers can write into it.
func clientAttrs(rep map[string]any) map[string]any {
	attrs, ok := rep["attributes"].(map[string]any)
	if !ok || attrs == nil {
		attrs = map[string]any{}
		rep["attributes"] = attrs
	}
	return attrs
}

// splitPostLogout parses the `##`-joined post-logout attribute string, dropping
// empties and the `+` sentinel (Keycloak's "same as redirect URIs" marker).
func splitPostLogout(v any) []string {
	s, _ := v.(string)
	var out []string
	for _, p := range strings.Split(s, postLogoutSep) {
		if p == "" || p == "+" {
			continue
		}
		out = append(out, p)
	}
	return out
}

// addURI returns uris with want appended iff absent, and whether it was added.
func addURI(uris []string, want string) ([]string, bool) {
	for _, u := range uris {
		if u == want {
			return uris, false
		}
	}
	return append(uris, want), true
}

// removeURI returns uris without want, and whether it was present.
func removeURI(uris []string, want string) ([]string, bool) {
	out := make([]string, 0, len(uris))
	removed := false
	for _, u := range uris {
		if u == want {
			removed = true
			continue
		}
		out = append(out, u)
	}
	return out, removed
}

func (c *Client) RegisterOrgRedirectURIs(ctx context.Context, redirectURI, postLogoutURI string) error {
	return c.editDashboardClient(ctx, func(rep map[string]any) bool {
		changed := false
		if uris, added := addURI(asStringSlice(rep["redirectUris"]), redirectURI); added {
			rep["redirectUris"] = uris
			changed = true
		}
		attrs := clientAttrs(rep)
		if pl, added := addURI(splitPostLogout(attrs["post.logout.redirect.uris"]), postLogoutURI); added {
			attrs["post.logout.redirect.uris"] = strings.Join(pl, postLogoutSep)
			changed = true
		}
		return changed
	})
}

func (c *Client) RemoveOrgRedirectURIs(ctx context.Context, redirectURI, postLogoutURI string) error {
	return c.editDashboardClient(ctx, func(rep map[string]any) bool {
		changed := false
		if uris, removed := removeURI(asStringSlice(rep["redirectUris"]), redirectURI); removed {
			rep["redirectUris"] = uris
			changed = true
		}
		attrs := clientAttrs(rep)
		if pl, removed := removeURI(splitPostLogout(attrs["post.logout.redirect.uris"]), postLogoutURI); removed {
			attrs["post.logout.redirect.uris"] = strings.Join(pl, postLogoutSep)
			changed = true
		}
		return changed
	})
}
