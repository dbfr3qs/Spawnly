package keycloak

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/spawnly/platform/internal/orgdirectory"
)

// fakeKC is a minimal in-memory stand-in for Keycloak's admin REST + token
// endpoints, exercising the exact request/response shapes this adapter depends on
// (verified against a live Keycloak 26.4 realm). It is deliberately small: enough
// to prove resolution (alias->uuid, username->uuid), idempotency (409/existing),
// and the raw-string member body.
type fakeKC struct {
	realm            string
	tokens           int                  // count of token mints (asserts caching)
	orgs             map[string]*fakeOrg  // uuid -> org
	users            map[string]*fakeUser // uuid -> user
	nextID           int
	forceTokenStatus int // if non-zero, token endpoint returns this

	// dashboard OIDC client (org-subdomains Phase 2): the client whose redirect /
	// post-logout URIs the satellite feature edits. dashRep is the FULL client
	// representation the adapter round-trips (GET then PUT).
	dashClientID string
	dashUUID     string
	dashRep      map[string]any
	dashPuts     int // count of PUT /clients/<uuid> (asserts idempotent skip)
}

type fakeOrg struct {
	id, alias, name string
	members         map[string]bool // user uuid set
}

type fakeUser struct {
	id, username, email string
	required            []string
}

func newFakeKC(realm string) *fakeKC {
	return &fakeKC{
		realm:        realm,
		orgs:         map[string]*fakeOrg{},
		users:        map[string]*fakeUser{},
		dashClientID: "dashboard",
		dashUUID:     "client-dashboard-uuid",
		// A representative starting rep: a pre-existing apex callback + an unrelated
		// field the adapter must PRESERVE across the PUT (Keycloak's PUT replaces the
		// whole representation).
		dashRep: map[string]any{
			"id":           "client-dashboard-uuid",
			"clientId":     "dashboard",
			"name":         "Spawnly Dashboard",
			"redirectUris": []any{"https://spawnly.run/callback"},
			"attributes": map[string]any{
				"post.logout.redirect.uris": "https://spawnly.run/",
			},
		},
	}
}

func (f *fakeKC) newID(prefix string) string {
	f.nextID++
	return prefix + "-" + string(rune('a'+f.nextID))
}

func (f *fakeKC) addUser(username, email string) *fakeUser {
	u := &fakeUser{id: f.newID("user"), username: username, email: email}
	f.users[u.id] = u
	return u
}

func (f *fakeKC) userByName(name string) *fakeUser {
	for _, u := range f.users {
		if u.username == name {
			return u
		}
	}
	return nil
}

func (f *fakeKC) orgByAlias(alias string) *fakeOrg {
	for _, o := range f.orgs {
		if o.alias == alias {
			return o
		}
	}
	return nil
}

func (f *fakeKC) handler() http.Handler {
	mux := http.NewServeMux()
	tokenPath := "/realms/" + f.realm + "/protocol/openid-connect/token"
	mux.HandleFunc(tokenPath, func(w http.ResponseWriter, r *http.Request) {
		if f.forceTokenStatus != 0 {
			w.WriteHeader(f.forceTokenStatus)
			return
		}
		f.tokens++
		json.NewEncoder(w).Encode(map[string]any{"access_token": "tok", "expires_in": 300})
	})

	admin := "/admin/realms/" + f.realm
	// Organizations collection: POST create, GET search.
	mux.HandleFunc(admin+"/organizations", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			var body struct{ Name, Alias string }
			json.NewDecoder(r.Body).Decode(&body)
			if f.orgByAlias(body.Alias) != nil {
				w.WriteHeader(http.StatusConflict)
				return
			}
			o := &fakeOrg{id: f.newID("org"), alias: body.Alias, name: body.Name, members: map[string]bool{}}
			f.orgs[o.id] = o
			w.Header().Set("Location", "http://kc"+admin+"/organizations/"+o.id)
			w.WriteHeader(http.StatusCreated)
		case http.MethodGet:
			// The adapter resolves by alias via q=alias:<alias>.
			q := r.URL.Query().Get("q")
			wantAlias := strings.TrimPrefix(q, "alias:")
			var out []map[string]string
			for _, o := range f.orgs {
				if q == "" || o.alias == wantAlias {
					out = append(out, map[string]string{"id": o.id, "alias": o.alias, "name": o.name})
				}
			}
			json.NewEncoder(w).Encode(out)
		}
	})
	// Users collection: POST create, GET username lookup.
	mux.HandleFunc(admin+"/users", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			var body struct {
				Username, Email string
				RequiredActions []string `json:"requiredActions"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			u := f.addUser(body.Username, body.Email)
			u.required = body.RequiredActions
			w.Header().Set("Location", "http://kc"+admin+"/users/"+u.id)
			w.WriteHeader(http.StatusCreated)
		case http.MethodGet:
			name := r.URL.Query().Get("username")
			var out []map[string]string
			if u := f.userByName(name); u != nil {
				out = append(out, map[string]string{"id": u.id, "username": u.username, "email": u.email})
			}
			json.NewEncoder(w).Encode(out)
		}
	})
	// Clients collection: GET ?clientId= lookup (dashboard resolution).
	mux.HandleFunc(admin+"/clients", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		var out []map[string]string
		if r.URL.Query().Get("clientId") == f.dashClientID {
			out = append(out, map[string]string{"id": f.dashUUID, "clientId": f.dashClientID})
		}
		json.NewEncoder(w).Encode(out)
	})
	// Single client: GET the full rep, PUT the (replaced) rep.
	mux.HandleFunc(admin+"/clients/", func(w http.ResponseWriter, r *http.Request) {
		uuid := strings.TrimPrefix(r.URL.Path, admin+"/clients/")
		if uuid != f.dashUUID {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		switch r.Method {
		case http.MethodGet:
			json.NewEncoder(w).Encode(f.dashRep)
		case http.MethodPut:
			f.dashPuts++
			var rep map[string]any
			if err := json.NewDecoder(r.Body).Decode(&rep); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			f.dashRep = rep // PUT REPLACES the representation
			w.WriteHeader(http.StatusNoContent)
		}
	})
	// Per-org and per-user subresources.
	mux.HandleFunc(admin+"/organizations/", func(w http.ResponseWriter, r *http.Request) {
		rest := strings.TrimPrefix(r.URL.Path, admin+"/organizations/")
		parts := strings.Split(rest, "/")
		// members/{uuid}/organizations
		if parts[0] == "members" && len(parts) == 3 && parts[2] == "organizations" {
			uid := parts[1]
			var out []map[string]string
			for _, o := range f.orgs {
				if o.members[uid] {
					out = append(out, map[string]string{"id": o.id, "alias": o.alias, "name": o.name})
				}
			}
			json.NewEncoder(w).Encode(out)
			return
		}
		orgID := parts[0]
		o := f.orgs[orgID]
		// /organizations/{id}
		if len(parts) == 1 {
			if r.Method == http.MethodDelete {
				if o == nil {
					w.WriteHeader(http.StatusNotFound)
					return
				}
				delete(f.orgs, orgID)
				w.WriteHeader(http.StatusNoContent)
			}
			return
		}
		if o == nil {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		// /organizations/{id}/members[/{uuid}]
		if parts[1] == "members" {
			if len(parts) == 2 {
				switch r.Method {
				case http.MethodPost:
					raw, _ := io.ReadAll(r.Body)
					var uid string
					json.Unmarshal(raw, &uid) // raw JSON string body
					if o.members[uid] {
						w.WriteHeader(http.StatusConflict)
						return
					}
					o.members[uid] = true
					w.WriteHeader(http.StatusCreated)
				case http.MethodGet:
					var out []map[string]string
					for uid := range o.members {
						u := f.users[uid]
						out = append(out, map[string]string{"id": u.id, "username": u.username, "email": u.email})
					}
					json.NewEncoder(w).Encode(out)
				}
				return
			}
			// members/{uuid} DELETE
			uid := parts[2]
			if r.Method == http.MethodDelete {
				if !o.members[uid] {
					w.WriteHeader(http.StatusNotFound)
					return
				}
				delete(o.members, uid)
				w.WriteHeader(http.StatusNoContent)
			}
		}
	})
	return mux
}

func newTestClient(t *testing.T) (*Client, *fakeKC) {
	t.Helper()
	f := newFakeKC("spawnly")
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)
	return New(srv.URL, "spawnly", "org-admin", "secret", "dashboard", srv.Client()), f
}

func TestCreateOrg_Idempotent(t *testing.T) {
	c, f := newTestClient(t)
	ctx := context.Background()
	if err := c.CreateOrg(ctx, "acme", "Acme"); err != nil {
		t.Fatalf("create: %v", err)
	}
	if o := f.orgByAlias("acme"); o == nil || o.name != "Acme" {
		t.Fatalf("org not created: %+v", o)
	}
	// Second create is a no-op success (idempotent retry).
	if err := c.CreateOrg(ctx, "acme", "Acme"); err != nil {
		t.Fatalf("second create should be idempotent: %v", err)
	}
	if n := len(f.orgs); n != 1 {
		t.Fatalf("expected 1 org, got %d", n)
	}
	// Token was minted once and reused across all calls.
	if f.tokens != 1 {
		t.Fatalf("expected 1 token mint (cached), got %d", f.tokens)
	}
}

func TestAddMember_CreatesUnknownUserWithPasswordReset(t *testing.T) {
	c, f := newTestClient(t)
	ctx := context.Background()
	if err := c.CreateOrg(ctx, "acme", "Acme"); err != nil {
		t.Fatal(err)
	}
	if err := c.AddMember(ctx, "acme", "newbie"); err != nil {
		t.Fatalf("add member: %v", err)
	}
	u := f.userByName("newbie")
	if u == nil {
		t.Fatal("unknown user was not created")
	}
	if len(u.required) != 1 || u.required[0] != "UPDATE_PASSWORD" {
		t.Fatalf("expected UPDATE_PASSWORD required action, got %v", u.required)
	}
	if !f.orgByAlias("acme").members[u.id] {
		t.Fatal("user not added as org member")
	}
	// Idempotent re-add: still one member, no error.
	if err := c.AddMember(ctx, "acme", "newbie"); err != nil {
		t.Fatalf("re-add should be idempotent: %v", err)
	}
}

func TestAddMember_ExistingUserNotRecreated(t *testing.T) {
	c, f := newTestClient(t)
	ctx := context.Background()
	f.addUser("alice", "alice@example.com")
	c.CreateOrg(ctx, "acme", "Acme")
	if err := c.AddMember(ctx, "acme", "alice"); err != nil {
		t.Fatal(err)
	}
	if n := len(f.users); n != 1 {
		t.Fatalf("existing user should not be recreated, have %d users", n)
	}
}

func TestListMembersAndUserOrgs(t *testing.T) {
	c, f := newTestClient(t)
	ctx := context.Background()
	c.CreateOrg(ctx, "acme", "Acme")
	c.CreateOrg(ctx, "globex", "Globex")
	c.AddMember(ctx, "acme", "alice")
	c.AddMember(ctx, "acme", "bob")
	c.AddMember(ctx, "globex", "bob")

	members, err := c.ListMembers(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, m := range members {
		names = append(names, m.UserID)
		if m.Role != "" {
			t.Errorf("Keycloak adapter must not report a role, got %q", m.Role)
		}
	}
	sort.Strings(names)
	if strings.Join(names, ",") != "alice,bob" {
		t.Fatalf("acme members = %v, want [alice bob]", names)
	}

	orgs, err := c.ListUserOrgs(ctx, "bob")
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(orgs)
	if strings.Join(orgs, ",") != "acme,globex" {
		t.Fatalf("bob orgs = %v, want [acme globex]", orgs)
	}
	_ = f
}

func TestListUserOrgs_UnknownUserEmptyNoError(t *testing.T) {
	c, _ := newTestClient(t)
	orgs, err := c.ListUserOrgs(context.Background(), "ghost")
	if err != nil {
		t.Fatalf("unknown user should not error: %v", err)
	}
	if len(orgs) != 0 {
		t.Fatalf("unknown user should be in no orgs, got %v", orgs)
	}
}

func TestRemoveMember_Idempotent(t *testing.T) {
	c, f := newTestClient(t)
	ctx := context.Background()
	c.CreateOrg(ctx, "acme", "Acme")
	c.AddMember(ctx, "acme", "alice")
	if err := c.RemoveMember(ctx, "acme", "alice"); err != nil {
		t.Fatal(err)
	}
	if len(f.orgByAlias("acme").members) != 0 {
		t.Fatal("member not removed")
	}
	// Removing a non-member converges to success.
	if err := c.RemoveMember(ctx, "acme", "alice"); err != nil {
		t.Fatalf("removing a non-member should be idempotent: %v", err)
	}
	// User account survives removal.
	if f.userByName("alice") == nil {
		t.Fatal("RemoveMember must not delete the user account")
	}
}

func TestSetRole_ValidatesSubjectAndRole(t *testing.T) {
	c, f := newTestClient(t)
	ctx := context.Background()
	f.addUser("alice", "")
	if err := c.SetRole(ctx, "acme", "alice", orgdirectory.RoleAdmin); err != nil {
		t.Fatalf("set role for existing user: %v", err)
	}
	if err := c.SetRole(ctx, "acme", "ghost", orgdirectory.RoleAdmin); err == nil {
		t.Fatal("set role for unknown user should error")
	}
	if err := c.SetRole(ctx, "acme", "alice", "owner"); err == nil {
		t.Fatal("invalid role should error")
	}
}

func TestDeleteOrg_Idempotent(t *testing.T) {
	c, f := newTestClient(t)
	ctx := context.Background()
	c.CreateOrg(ctx, "acme", "Acme")
	if err := c.DeleteOrg(ctx, "acme"); err != nil {
		t.Fatal(err)
	}
	if len(f.orgs) != 0 {
		t.Fatal("org not deleted")
	}
	if err := c.DeleteOrg(ctx, "acme"); err != nil {
		t.Fatalf("deleting an absent org should be idempotent: %v", err)
	}
}

func TestTokenFailurePropagates(t *testing.T) {
	c, f := newTestClient(t)
	f.forceTokenStatus = http.StatusUnauthorized
	if err := c.CreateOrg(context.Background(), "acme", "Acme"); err == nil {
		t.Fatal("expected error when token endpoint fails")
	}
}

// dashRedirects returns the dashboard client's current redirect URIs.
func (f *fakeKC) dashRedirects() []string {
	arr, _ := f.dashRep["redirectUris"].([]any)
	var out []string
	for _, e := range arr {
		if s, ok := e.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// dashPostLogout returns the dashboard client's current post-logout URIs (the
// `##`-joined attribute, split for assertion).
func (f *fakeKC) dashPostLogout() []string {
	attrs, _ := f.dashRep["attributes"].(map[string]any)
	s, _ := attrs["post.logout.redirect.uris"].(string)
	if s == "" {
		return nil
	}
	return strings.Split(s, "##")
}

func contains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

func TestRegisterOrgRedirectURIs_AddsAndIsIdempotent(t *testing.T) {
	c, f := newTestClient(t)
	ctx := context.Background()
	redir := "https://acme.localtest.me/callback"
	logout := "https://acme.localtest.me/"

	if err := c.RegisterOrgRedirectURIs(ctx, redir, logout); err != nil {
		t.Fatalf("register: %v", err)
	}
	if !contains(f.dashRedirects(), redir) {
		t.Fatalf("redirect not added: %v", f.dashRedirects())
	}
	if !contains(f.dashPostLogout(), logout) {
		t.Fatalf("post-logout not added: %v", f.dashPostLogout())
	}
	// The pre-existing apex URIs survive the PUT (full-rep round-trip).
	if !contains(f.dashRedirects(), "https://spawnly.run/callback") {
		t.Fatalf("pre-existing apex redirect lost: %v", f.dashRedirects())
	}
	if !contains(f.dashPostLogout(), "https://spawnly.run/") {
		t.Fatalf("pre-existing apex post-logout lost: %v", f.dashPostLogout())
	}
	// Unrelated field preserved.
	if f.dashRep["name"] != "Spawnly Dashboard" {
		t.Fatalf("unrelated field clobbered: %v", f.dashRep["name"])
	}

	puts := f.dashPuts
	// Re-register: no duplicate, and no PUT at all (nothing changed).
	if err := c.RegisterOrgRedirectURIs(ctx, redir, logout); err != nil {
		t.Fatalf("re-register: %v", err)
	}
	if got := countOf(f.dashRedirects(), redir); got != 1 {
		t.Fatalf("redirect duplicated: %d copies", got)
	}
	if f.dashPuts != puts {
		t.Fatalf("idempotent re-register should skip the PUT (was %d, now %d)", puts, f.dashPuts)
	}
}

func countOf(xs []string, want string) int {
	n := 0
	for _, x := range xs {
		if x == want {
			n++
		}
	}
	return n
}

func TestRemoveOrgRedirectURIs_RemovesOnlyTheOrgURIs(t *testing.T) {
	c, f := newTestClient(t)
	ctx := context.Background()
	redir := "https://acme.localtest.me/callback"
	logout := "https://acme.localtest.me/"
	if err := c.RegisterOrgRedirectURIs(ctx, redir, logout); err != nil {
		t.Fatal(err)
	}
	if err := c.RemoveOrgRedirectURIs(ctx, redir, logout); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if contains(f.dashRedirects(), redir) {
		t.Fatalf("org redirect not removed: %v", f.dashRedirects())
	}
	if contains(f.dashPostLogout(), logout) {
		t.Fatalf("org post-logout not removed: %v", f.dashPostLogout())
	}
	// The apex URIs are untouched.
	if !contains(f.dashRedirects(), "https://spawnly.run/callback") {
		t.Fatalf("apex redirect must remain: %v", f.dashRedirects())
	}
	if !contains(f.dashPostLogout(), "https://spawnly.run/") {
		t.Fatalf("apex post-logout must remain: %v", f.dashPostLogout())
	}
}

func TestRemoveOrgRedirectURIs_AbsentIsSuccess(t *testing.T) {
	c, f := newTestClient(t)
	puts := f.dashPuts
	// Never registered — removing converges to success and does not PUT.
	if err := c.RemoveOrgRedirectURIs(context.Background(),
		"https://ghost.localtest.me/callback", "https://ghost.localtest.me/"); err != nil {
		t.Fatalf("removing absent URIs should be idempotent: %v", err)
	}
	if f.dashPuts != puts {
		t.Fatalf("removing absent URIs should skip the PUT (was %d, now %d)", puts, f.dashPuts)
	}
}

func TestRegisterOrgRedirectURIs_BareRepCreatesFields(t *testing.T) {
	c, f := newTestClient(t)
	// A minimal client rep with neither redirectUris nor attributes — the adapter
	// must create both rather than panic on the nil maps/slices.
	f.dashRep = map[string]any{"id": f.dashUUID, "clientId": "dashboard"}
	redir := "https://acme.localtest.me/callback"
	logout := "https://acme.localtest.me/"
	if err := c.RegisterOrgRedirectURIs(context.Background(), redir, logout); err != nil {
		t.Fatalf("register onto bare rep: %v", err)
	}
	if !contains(f.dashRedirects(), redir) {
		t.Fatalf("redirect not added to bare rep: %v", f.dashRedirects())
	}
	if !contains(f.dashPostLogout(), logout) {
		t.Fatalf("post-logout not added to bare rep: %v", f.dashPostLogout())
	}
}

func TestEditDashboardClient_StaleUUIDReresolvedAfter404(t *testing.T) {
	c, f := newTestClient(t)
	ctx := context.Background()
	redir := "https://acme.localtest.me/callback"
	logout := "https://acme.localtest.me/"
	// Prime the UUID cache.
	if err := c.RegisterOrgRedirectURIs(ctx, redir, logout); err != nil {
		t.Fatal(err)
	}
	// Simulate a realm wipe: the dashboard client is recreated under a NEW uuid.
	newUUID := "client-dashboard-uuid-2"
	f.dashUUID = newUUID
	f.dashRep = map[string]any{"id": newUUID, "clientId": "dashboard"}
	// First call 404s on the stale cached uuid and clears the cache.
	if err := c.RemoveOrgRedirectURIs(ctx, redir, logout); err == nil {
		t.Fatal("expected a 404 error against the stale uuid")
	}
	// The retry re-resolves the fresh uuid and succeeds (idempotent no-op remove).
	if err := c.RemoveOrgRedirectURIs(ctx, redir, logout); err != nil {
		t.Fatalf("retry after cache invalidation should succeed: %v", err)
	}
}

func TestRegisterOrgRedirectURIs_MissingDashboardClientIsError(t *testing.T) {
	c, f := newTestClient(t)
	f.dashClientID = "not-dashboard" // GET ?clientId=dashboard now returns []
	if err := c.RegisterOrgRedirectURIs(context.Background(),
		"https://acme.localtest.me/callback", "https://acme.localtest.me/"); err == nil {
		t.Fatal("expected a hard error when the dashboard client is missing")
	}
}
