package keycloak

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/spawnly/platform/internal/orgdirectory"
)

// fakeKC is a minimal in-memory stand-in for Keycloak's admin REST + token
// endpoints, exercising the exact request/response shapes this adapter depends on
// (verified against a live Keycloak 26.4 realm). It is deliberately small: enough
// to prove resolution (alias->uuid, username->uuid), idempotency (409/existing),
// and the raw-string member body.
type fakeKC struct {
	// mu serializes every request the fake handles so its in-memory maps are safe
	// under the concurrent-lifecycle test. It is released between requests, so the
	// adapter's own per-user RMW lock (not this) is what prevents a lost update — a
	// GET and its follow-up PUT are separate lock acquisitions here, mirroring real
	// Keycloak (which has no cross-request lock).
	mu               sync.Mutex
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

	logouts  map[string]int // user uuid -> count of POST /users/<id>/logout
	userPuts int            // count of PUT /users/<id> (asserts idempotent skip)
}

type fakeOrg struct {
	id, alias, name string
	members         map[string]bool // user uuid set
}

type fakeUser struct {
	id, username, email string
	required            []string
	emailVerified       bool
	enabled             bool
	attrs               map[string][]string
}

func newFakeKC(realm string) *fakeKC {
	return &fakeKC{
		realm:        realm,
		orgs:         map[string]*fakeOrg{},
		users:        map[string]*fakeUser{},
		logouts:      map[string]int{},
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
	u := &fakeUser{id: f.newID("user"), username: username, email: email, enabled: true, attrs: map[string][]string{}}
	f.users[u.id] = u
	return u
}

// userRep renders a fakeUser as Keycloak's user representation (attributes as
// JSON string arrays), the shape ListUsers/editUser round-trip.
func (f *fakeKC) userRep(u *fakeUser) map[string]any {
	attrs := map[string]any{}
	for k, v := range u.attrs {
		arr := make([]any, len(v))
		for i, s := range v {
			arr[i] = s
		}
		attrs[k] = arr
	}
	return map[string]any{
		"id":         u.id,
		"username":   u.username,
		"email":      u.email,
		"enabled":    u.enabled,
		"attributes": attrs,
	}
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
	// Users collection: POST create, GET username lookup, GET paginated list.
	mux.HandleFunc(admin+"/users", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			var body struct {
				Username, Email string
				RequiredActions []string `json:"requiredActions"`
				EmailVerified   bool     `json:"emailVerified"`
				Enabled         bool     `json:"enabled"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			u := f.addUser(body.Username, body.Email)
			u.required = body.RequiredActions
			u.emailVerified = body.EmailVerified
			u.enabled = body.Enabled
			w.Header().Set("Location", "http://kc"+admin+"/users/"+u.id)
			w.WriteHeader(http.StatusCreated)
		case http.MethodGet:
			// username lookup (exact) — resolveUserID.
			if name := r.URL.Query().Get("username"); name != "" {
				var out []map[string]string
				if u := f.userByName(name); u != nil {
					out = append(out, map[string]string{"id": u.id, "username": u.username, "email": u.email})
				}
				json.NewEncoder(w).Encode(out)
				return
			}
			// Paginated full-list (ListUsers) — first/max over a stable sort so
			// pagination is deterministic.
			first, _ := strconv.Atoi(r.URL.Query().Get("first"))
			max, _ := strconv.Atoi(r.URL.Query().Get("max"))
			var ids []string
			for id := range f.users {
				ids = append(ids, id)
			}
			sort.Slice(ids, func(i, j int) bool { return f.users[ids[i]].username < f.users[ids[j]].username })
			out := []map[string]any{}
			for i := first; i < len(ids) && (max <= 0 || i < first+max); i++ {
				out = append(out, f.userRep(f.users[ids[i]]))
			}
			json.NewEncoder(w).Encode(out)
		}
	})
	// Single user: GET full rep, PUT (replaces enabled + attributes), DELETE, and
	// POST .../logout (session revoke).
	mux.HandleFunc(admin+"/users/", func(w http.ResponseWriter, r *http.Request) {
		rest := strings.TrimPrefix(r.URL.Path, admin+"/users/")
		parts := strings.Split(rest, "/")
		u := f.users[parts[0]]
		if u == nil {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if len(parts) == 2 && parts[1] == "logout" {
			if r.Method == http.MethodPost {
				f.logouts[u.id]++
				w.WriteHeader(http.StatusNoContent)
			}
			return
		}
		switch r.Method {
		case http.MethodGet:
			json.NewEncoder(w).Encode(f.userRep(u))
		case http.MethodPut:
			f.userPuts++
			var rep map[string]any
			if err := json.NewDecoder(r.Body).Decode(&rep); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if e, ok := rep["enabled"].(bool); ok {
				u.enabled = e
			}
			// PUT REPLACES attributes with exactly what was sent (read-modify-write).
			u.attrs = map[string][]string{}
			if attrs, ok := rep["attributes"].(map[string]any); ok {
				for k, v := range attrs {
					if arr, ok := v.([]any); ok {
						var ss []string
						for _, e := range arr {
							if s, ok := e.(string); ok {
								ss = append(ss, s)
							}
						}
						u.attrs[k] = ss
					}
				}
			}
			w.WriteHeader(http.StatusNoContent)
		case http.MethodDelete:
			delete(f.users, u.id)
			w.WriteHeader(http.StatusNoContent)
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
	// Serialize every request so the fake's maps are race-free under concurrency.
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		mux.ServeHTTP(w, r)
	})
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
	// Admin-created users are pinned emailVerified so they bypass the realm's
	// self-serve email-verification requirement (signup-lifecycle Phase 1).
	if !u.emailVerified {
		t.Fatal("admin-created user must be pinned emailVerified=true")
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

// ---- user lifecycle (signup-lifecycle Phase 3) ----------------------------

func TestListUsers_PaginatesAndReportsState(t *testing.T) {
	c, f := newTestClient(t)
	ctx := context.Background()
	// More than one page (pageSize=100) so the first/max walk is exercised.
	const n = 150
	for i := 0; i < n; i++ {
		f.addUser("user"+string(rune('a'+i%26))+strconv.Itoa(i), "")
	}
	// One disabled-by-org user to prove Enabled + DisabledByOrg surface.
	du := f.addUser("suspended", "s@example.com")
	du.enabled = false
	du.attrs["disabledByOrg"] = []string{"acme"}

	users, err := c.ListUsers(ctx)
	if err != nil {
		t.Fatalf("list users: %v", err)
	}
	if len(users) != n+1 {
		t.Fatalf("ListUsers returned %d, want %d (pagination truncated?)", len(users), n+1)
	}
	var found bool
	for _, u := range users {
		if u.Username == "suspended" {
			found = true
			if u.Enabled {
				t.Error("suspended user should report Enabled=false")
			}
			if u.DisabledByOrg != "acme" {
				t.Errorf("DisabledByOrg = %q, want acme", u.DisabledByOrg)
			}
			if u.Email != "s@example.com" {
				t.Errorf("Email = %q, want s@example.com", u.Email)
			}
		}
	}
	if !found {
		t.Fatal("suspended user missing from the paginated list")
	}
}

func TestDisableUser_SetsDisabledAndRevokesSessions(t *testing.T) {
	c, f := newTestClient(t)
	ctx := context.Background()
	u := f.addUser("alice", "")
	if err := c.DisableUser(ctx, "alice", ""); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if u.enabled {
		t.Error("user should be disabled (enabled=false)")
	}
	// A direct admin ban sets the SEPARATE disabledByAdmin marker, never disabledByOrg.
	if got := u.attrs["disabledByAdmin"]; len(got) != 1 || got[0] != "true" {
		t.Errorf("direct admin disable must set disabledByAdmin=[true], got %v", got)
	}
	if len(u.attrs["disabledByOrg"]) != 0 {
		t.Errorf("direct admin disable must not stamp disabledByOrg, got %v", u.attrs["disabledByOrg"])
	}
	if f.logouts[u.id] != 1 {
		t.Errorf("expected 1 session-revoke (logout), got %d", f.logouts[u.id])
	}
}

func TestDisableUser_MultiOrgAccumulatesSet(t *testing.T) {
	c, f := newTestClient(t)
	ctx := context.Background()
	u := f.addUser("bob", "")

	// Two orgs suspend bob → disabledByOrg holds BOTH (a set, not a single value).
	if err := c.DisableUser(ctx, "bob", "acme"); err != nil {
		t.Fatalf("disable byOrg acme: %v", err)
	}
	if err := c.DisableUser(ctx, "bob", "globex"); err != nil {
		t.Fatalf("disable byOrg globex: %v", err)
	}
	got := append([]string(nil), u.attrs["disabledByOrg"]...)
	sort.Strings(got)
	if strings.Join(got, ",") != "acme,globex" {
		t.Errorf("disabledByOrg = %v, want [acme globex]", u.attrs["disabledByOrg"])
	}
	if u.enabled {
		t.Error("user should be disabled")
	}
	// A repeat suspension by an org already in the set is a dedup no-op.
	if err := c.DisableUser(ctx, "bob", "acme"); err != nil {
		t.Fatalf("repeat disable byOrg acme: %v", err)
	}
	if n := len(u.attrs["disabledByOrg"]); n != 2 {
		t.Errorf("repeat org-disable must dedup, set now has %d entries: %v", n, u.attrs["disabledByOrg"])
	}
}

func TestDisableUser_ByOrgStampsTagAndPreservesAttrs(t *testing.T) {
	c, f := newTestClient(t)
	ctx := context.Background()
	u := f.addUser("bob", "")
	u.attrs["unrelated"] = []string{"keepme"} // must survive the read-modify-write
	if err := c.DisableUser(ctx, "bob", "acme"); err != nil {
		t.Fatalf("disable byOrg: %v", err)
	}
	if u.enabled {
		t.Error("user should be disabled")
	}
	if got := u.attrs["disabledByOrg"]; len(got) != 1 || got[0] != "acme" {
		t.Errorf("disabledByOrg = %v, want [acme]", got)
	}
	if got := u.attrs["unrelated"]; len(got) != 1 || got[0] != "keepme" {
		t.Errorf("unrelated attribute clobbered by RMW: %v", got)
	}
}

// A member disabled for NO tracked reason (legacy/out-of-band disable) is preserved
// as an admin ban when an org later suspends them, so the org-enable that empties
// the set can't launder the pre-existing lockout away.
func TestDisableUser_LegacyDisablePreservedAsBan(t *testing.T) {
	c, f := newTestClient(t)
	ctx := context.Background()
	u := f.addUser("legacy", "")
	u.enabled = false // disabled out-of-band, with NO attributes

	if err := c.DisableUser(ctx, "legacy", "acme"); err != nil {
		t.Fatalf("org-disable legacy user: %v", err)
	}
	if got := u.attrs["disabledByAdmin"]; len(got) != 1 || got[0] != "true" {
		t.Errorf("legacy disable should be healed to an admin ban, got %v", got)
	}
	if got := u.attrs["disabledByOrg"]; len(got) != 1 || got[0] != "acme" {
		t.Errorf("org tag should also be recorded, got %v", got)
	}
	// Org-enable empties the set but the healed admin ban keeps them disabled.
	if _, err := c.EnableUser(ctx, "legacy", "acme"); err != nil {
		t.Fatalf("org-enable: %v", err)
	}
	if u.enabled {
		t.Error("legacy-disabled user must STAY disabled after the org lifts its suspension")
	}
}

// Concurrent lifecycle mutations on the SAME user must not lose a suspension reason
// (the per-user RMW lock closes the last-PUT-wins race). Two org suspensions + a
// direct ban race; every reason must survive. Run under `go test -race`.
func TestDisableUser_ConcurrentReasonsNotLost(t *testing.T) {
	c, f := newTestClient(t)
	ctx := context.Background()
	u := f.addUser("racer", "")

	var wg sync.WaitGroup
	for _, byOrg := range []string{"orgA", "orgB", ""} {
		wg.Add(1)
		go func(b string) {
			defer wg.Done()
			if err := c.DisableUser(ctx, "racer", b); err != nil {
				t.Errorf("DisableUser(%q): %v", b, err)
			}
		}(byOrg)
	}
	wg.Wait()

	set := append([]string(nil), u.attrs["disabledByOrg"]...)
	sort.Strings(set)
	if strings.Join(set, ",") != "orgA,orgB" {
		t.Errorf("disabledByOrg = %v, want [orgA orgB] (a concurrent reason was lost)", u.attrs["disabledByOrg"])
	}
	if got := u.attrs["disabledByAdmin"]; len(got) != 1 || got[0] != "true" {
		t.Errorf("disabledByAdmin = %v, want [true] (the concurrent ban was lost)", got)
	}
	if u.enabled {
		t.Error("user must be disabled")
	}
}

func TestDisableUser_UnknownUserNoOp(t *testing.T) {
	c, _ := newTestClient(t)
	if err := c.DisableUser(context.Background(), "ghost", ""); err != nil {
		t.Fatalf("disabling an unknown user should be a no-op success: %v", err)
	}
}

func TestEnableUser_DirectAdminClearsBanMarker(t *testing.T) {
	c, f := newTestClient(t)
	ctx := context.Background()
	u := f.addUser("carol", "")
	u.enabled = false
	u.attrs["disabledByAdmin"] = []string{"true"}
	applied, err := c.EnableUser(ctx, "carol", "")
	if err != nil {
		t.Fatalf("enable: %v", err)
	}
	if !applied {
		t.Error("direct admin enable of an admin-banned user should report applied=true")
	}
	if !u.enabled {
		t.Error("user should be enabled")
	}
	if len(u.attrs["disabledByAdmin"]) != 0 {
		t.Errorf("disabledByAdmin marker should be cleared, got %v", u.attrs["disabledByAdmin"])
	}
}

// A direct admin enable clears the ban marker but must NOT resurrect an account an
// org still suspends (the ban can't be laundered into an org-enable).
func TestEnableUser_DirectAdminRespectsOrgSuspension(t *testing.T) {
	c, f := newTestClient(t)
	ctx := context.Background()
	u := f.addUser("dana", "")
	u.enabled = false
	u.attrs["disabledByAdmin"] = []string{"true"}
	u.attrs["disabledByOrg"] = []string{"acme"}

	applied, err := c.EnableUser(ctx, "dana", "")
	if err != nil {
		t.Fatalf("enable: %v", err)
	}
	if !applied {
		t.Error("clearing the ban marker is a change → applied=true")
	}
	if len(u.attrs["disabledByAdmin"]) != 0 {
		t.Errorf("ban marker should be cleared, got %v", u.attrs["disabledByAdmin"])
	}
	if u.enabled {
		t.Error("account must STAY disabled while org acme still suspends it")
	}
	if got := u.attrs["disabledByOrg"]; len(got) != 1 || got[0] != "acme" {
		t.Errorf("org suspension must remain, got %v", got)
	}
}

// The mirror invariant: an org lifting its suspension must not launder away a
// concurrent admin ban.
func TestEnableUser_OrgEnableRespectsAdminBan(t *testing.T) {
	c, f := newTestClient(t)
	ctx := context.Background()
	u := f.addUser("erin", "")
	u.enabled = false
	u.attrs["disabledByAdmin"] = []string{"true"}
	u.attrs["disabledByOrg"] = []string{"acme"}

	applied, err := c.EnableUser(ctx, "erin", "acme")
	if err != nil {
		t.Fatalf("org enable: %v", err)
	}
	if !applied {
		t.Error("removing acme from the set is a change → applied=true")
	}
	if len(u.attrs["disabledByOrg"]) != 0 {
		t.Errorf("acme should be removed from the set, got %v", u.attrs["disabledByOrg"])
	}
	if u.enabled {
		t.Error("account must STAY disabled while the admin ban is in force")
	}
	if got := u.attrs["disabledByAdmin"]; len(got) != 1 || got[0] != "true" {
		t.Errorf("admin ban must remain, got %v", got)
	}
}

// A member suspended by two orgs stays disabled until BOTH lift; enabling only when
// the set is finally empty.
func TestEnableUser_MultiOrgStaysDisabledUntilAllLift(t *testing.T) {
	c, f := newTestClient(t)
	ctx := context.Background()
	u := f.addUser("frank", "")
	u.enabled = false
	u.attrs["disabledByOrg"] = []string{"acme", "globex"}

	// acme lifts → still suspended by globex.
	if _, err := c.EnableUser(ctx, "frank", "acme"); err != nil {
		t.Fatalf("enable acme: %v", err)
	}
	if u.enabled {
		t.Error("frank must stay disabled while globex still suspends him")
	}
	if got := u.attrs["disabledByOrg"]; len(got) != 1 || got[0] != "globex" {
		t.Errorf("set after acme-enable = %v, want [globex]", got)
	}
	// globex lifts → set empty → enabled.
	if _, err := c.EnableUser(ctx, "frank", "globex"); err != nil {
		t.Fatalf("enable globex: %v", err)
	}
	if !u.enabled {
		t.Error("frank should be enabled once every org has lifted its suspension")
	}
	if len(u.attrs["disabledByOrg"]) != 0 {
		t.Errorf("set should be empty, got %v", u.attrs["disabledByOrg"])
	}
}

func TestEnableUser_TagMatchVsMismatch(t *testing.T) {
	c, f := newTestClient(t)
	ctx := context.Background()

	// Tag matches the org → enable + clear.
	m := f.addUser("match", "")
	m.enabled = false
	m.attrs["disabledByOrg"] = []string{"acme"}
	applied, err := c.EnableUser(ctx, "match", "acme")
	if err != nil {
		t.Fatal(err)
	}
	if !applied || !m.enabled || len(m.attrs["disabledByOrg"]) != 0 {
		t.Errorf("tag-match enable: applied=%v enabled=%v tag=%v", applied, m.enabled, m.attrs["disabledByOrg"])
	}

	// Independently disabled (no tag) → org-enable must NOT touch it.
	indep := f.addUser("indep", "")
	indep.enabled = false
	applied, err = c.EnableUser(ctx, "indep", "acme")
	if err != nil {
		t.Fatal(err)
	}
	if applied {
		t.Error("org-enable of an untagged user should report applied=false")
	}
	if indep.enabled {
		t.Error("independently-disabled user must stay disabled on a tag-mismatch enable")
	}

	// Disabled by a DIFFERENT org → this org's enable must NOT touch it.
	other := f.addUser("other", "")
	other.enabled = false
	other.attrs["disabledByOrg"] = []string{"globex"}
	applied, err = c.EnableUser(ctx, "other", "acme")
	if err != nil {
		t.Fatal(err)
	}
	if applied || other.enabled || other.attrs["disabledByOrg"][0] != "globex" {
		t.Errorf("cross-org enable leaked: applied=%v enabled=%v tag=%v", applied, other.enabled, other.attrs["disabledByOrg"])
	}
}

func TestEnableUser_UnknownUser(t *testing.T) {
	c, _ := newTestClient(t)
	applied, err := c.EnableUser(context.Background(), "ghost", "")
	if err != nil {
		t.Fatalf("enabling an unknown user should not error: %v", err)
	}
	if applied {
		t.Error("unknown user enable should report applied=false")
	}
}

func TestDeleteUser_Idempotent(t *testing.T) {
	c, f := newTestClient(t)
	ctx := context.Background()
	u := f.addUser("dave", "")
	if err := c.DeleteUser(ctx, "dave"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, ok := f.users[u.id]; ok {
		t.Fatal("user not deleted")
	}
	// Deleting an absent user converges to success.
	if err := c.DeleteUser(ctx, "dave"); err != nil {
		t.Fatalf("deleting an absent user should be idempotent: %v", err)
	}
}
