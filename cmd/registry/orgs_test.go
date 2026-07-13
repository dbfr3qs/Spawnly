package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/spawnly/platform/internal/controlplane"
	"github.com/spawnly/platform/internal/orgdirectory"
	"github.com/spawnly/platform/internal/registrant"
	"github.com/spawnly/platform/internal/registry"
	"github.com/spawnly/platform/internal/spicedb"
	"github.com/spawnly/platform/internal/spiffe"
)

// fakeDirectory is an in-memory orgdirectory.Directory for registry handler
// tests, with per-method failure injection so the dual-write cleanup/unwind path
// can be proven. It records every call so a test can assert an unwind happened
// (e.g. DeleteOrg after a failed AddMember).
type fakeDirectory struct {
	mu      sync.Mutex
	orgs    map[string]bool            // alias -> exists
	members map[string]map[string]bool // alias -> set of userIds
	calls   []string

	// User lifecycle state (signup-lifecycle Phase 3). users tracks each known
	// user's enabled flag + disabledByOrg tag; logouts counts session revokes.
	users   map[string]*fakeUserState // username -> state
	logouts map[string]int            // username -> revoke count

	failCreateOrg    error
	failAddMember    error
	failRemoveMember error
	failDeleteOrg    error
	failSetRole      error
	failListMembers  error
	failRegisterURIs error
	failListUsers    error
	failDisableUser  error
	failEnableUser   error
	failDeleteUser   error
}

// fakeUserState mirrors the real Keycloak adapter's disable-state model: a
// multi-valued disabledByOrg SET plus a SEPARATE direct-admin ban marker. A user
// is enabled iff NOT admin-banned AND the org set is empty (see keycloak.go).
type fakeUserState struct {
	email           string
	enabled         bool
	disabledByOrg   []string // set of orgs currently suspending the user
	disabledByAdmin bool     // direct platform-admin ban
}

// hasOrg reports whether org is in the user's suspending-org set.
func (u *fakeUserState) hasOrg(org string) bool { return containsStr(u.disabledByOrg, org) }

func newFakeDirectory() *fakeDirectory {
	return &fakeDirectory{
		orgs:    map[string]bool{},
		members: map[string]map[string]bool{},
		users:   map[string]*fakeUserState{},
		logouts: map[string]int{},
	}
}

// ensureUser returns (creating if needed) the tracked state for username, so a
// test that adds a member or references a user directly sees a consistent record.
func (f *fakeDirectory) ensureUser(username string) *fakeUserState {
	u := f.users[username]
	if u == nil {
		u = &fakeUserState{enabled: true}
		f.users[username] = u
	}
	return u
}

// testDir returns a permissive fake directory — used by the many existing
// buildMux call sites that never touch the org routes.
func testDir() orgdirectory.Directory { return newFakeDirectory() }

func (f *fakeDirectory) record(s string) { f.calls = append(f.calls, s) }

func (f *fakeDirectory) hasCall(s string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if c == s {
			return true
		}
	}
	return false
}

func (f *fakeDirectory) hasCallPrefix(prefix string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}

func (f *fakeDirectory) CreateOrg(_ context.Context, orgID, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("CreateOrg:" + orgID)
	if f.failCreateOrg != nil {
		return f.failCreateOrg
	}
	f.orgs[orgID] = true
	if f.members[orgID] == nil {
		f.members[orgID] = map[string]bool{}
	}
	return nil
}

func (f *fakeDirectory) DeleteOrg(_ context.Context, orgID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("DeleteOrg:" + orgID)
	if f.failDeleteOrg != nil {
		return f.failDeleteOrg
	}
	delete(f.orgs, orgID)
	delete(f.members, orgID)
	return nil
}

func (f *fakeDirectory) AddMember(_ context.Context, orgID, user string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("AddMember:" + orgID + "/" + user)
	if f.failAddMember != nil {
		return f.failAddMember
	}
	if f.members[orgID] == nil {
		f.members[orgID] = map[string]bool{}
	}
	f.members[orgID][user] = true
	f.ensureUser(user) // a member is a known user (enabled by default)
	return nil
}

func (f *fakeDirectory) RemoveMember(_ context.Context, orgID, user string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("RemoveMember:" + orgID + "/" + user)
	if f.failRemoveMember != nil {
		return f.failRemoveMember
	}
	if f.members[orgID] != nil {
		delete(f.members[orgID], user)
	}
	return nil
}

func (f *fakeDirectory) SetRole(_ context.Context, orgID, user, role string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("SetRole:" + orgID + "/" + user + "=" + role)
	return f.failSetRole
}

func (f *fakeDirectory) ListMembers(_ context.Context, orgID string) ([]orgdirectory.Member, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failListMembers != nil {
		return nil, f.failListMembers
	}
	var out []orgdirectory.Member
	for u := range f.members[orgID] {
		enabled := true
		if st := f.users[u]; st != nil {
			enabled = st.enabled
		}
		out = append(out, orgdirectory.Member{UserID: u, Username: u, Enabled: enabled})
	}
	return out, nil
}

func (f *fakeDirectory) ListUserOrgs(_ context.Context, user string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for org, mem := range f.members {
		if mem[user] {
			out = append(out, org)
		}
	}
	sort.Strings(out)
	return out, nil
}

func (f *fakeDirectory) RegisterOrgRedirectURIs(_ context.Context, redirectURI, postLogoutURI string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("RegisterOrgRedirectURIs:" + redirectURI + "|" + postLogoutURI)
	return f.failRegisterURIs
}

func (f *fakeDirectory) RemoveOrgRedirectURIs(_ context.Context, redirectURI, postLogoutURI string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("RemoveOrgRedirectURIs:" + redirectURI + "|" + postLogoutURI)
	return nil
}

// ── User lifecycle (signup-lifecycle Phase 3) ───────────────────────────────

func (f *fakeDirectory) ListUsers(_ context.Context) ([]orgdirectory.User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failListUsers != nil {
		return nil, f.failListUsers
	}
	out := make([]orgdirectory.User, 0, len(f.users))
	for name, u := range f.users {
		// The adapter surfaces the FIRST org in the set (firstAttr); mirror that.
		firstOrg := ""
		if len(u.disabledByOrg) > 0 {
			firstOrg = u.disabledByOrg[0]
		}
		out = append(out, orgdirectory.User{
			Username:      name,
			Email:         u.email,
			Enabled:       u.enabled,
			DisabledByOrg: firstOrg,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Username < out[j].Username })
	return out, nil
}

func (f *fakeDirectory) DisableUser(_ context.Context, username, byOrg string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("DisableUser:" + username + "|" + byOrg)
	if f.failDisableUser != nil {
		return f.failDisableUser
	}
	u := f.ensureUser(username)
	// Mirror the adapter: an org disable ADDS to the multi-valued set (dedup); a
	// direct admin disable (byOrg=="") sets the separate ban marker. A disable for
	// ANY reason blocks login.
	if byOrg != "" {
		// Mirror the adapter's migration guard: a member already disabled for NO
		// tracked reason (a legacy/out-of-band disable) is preserved as an admin ban
		// before the org tag is added, so a later org-enable can't launder it away.
		if !u.enabled && len(u.disabledByOrg) == 0 && !u.disabledByAdmin {
			u.disabledByAdmin = true
		}
		if !u.hasOrg(byOrg) {
			u.disabledByOrg = append(u.disabledByOrg, byOrg)
		}
	} else {
		u.disabledByAdmin = true
	}
	u.enabled = false
	f.logouts[username]++
	return nil
}

func (f *fakeDirectory) EnableUser(_ context.Context, username, onlyIfDisabledByOrg string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("EnableUser:" + username + "|" + onlyIfDisabledByOrg)
	if f.failEnableUser != nil {
		return false, f.failEnableUser
	}
	u := f.users[username]
	if u == nil {
		return false, nil
	}
	changed := false
	if onlyIfDisabledByOrg != "" {
		// Org enable: remove this org from the set; a no-op if it wasn't ours.
		if !u.hasOrg(onlyIfDisabledByOrg) {
			return false, nil
		}
		out := u.disabledByOrg[:0]
		for _, o := range u.disabledByOrg {
			if o != onlyIfDisabledByOrg {
				out = append(out, o)
			}
		}
		u.disabledByOrg = out
		changed = true
	} else if u.disabledByAdmin {
		u.disabledByAdmin = false // direct admin enable clears the ban marker
		changed = true
	}
	// Invariant: enabled iff NOT admin-banned AND no org still suspends them.
	wantEnabled := !u.disabledByAdmin && len(u.disabledByOrg) == 0
	if u.enabled != wantEnabled {
		u.enabled = wantEnabled
		changed = true
	}
	return changed, nil
}

func (f *fakeDirectory) DeleteUser(_ context.Context, username string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("DeleteUser:" + username)
	if f.failDeleteUser != nil {
		return f.failDeleteUser
	}
	delete(f.users, username)
	return nil
}

// flakySpice wraps spicedb.Mock to inject a WriteRelationship failure, so the
// member-add tuple-write unwind can be exercised (the Mock never errors).
type flakySpice struct {
	*spicedb.Mock
	failWrite bool
}

func (s *flakySpice) WriteRelationship(ctx context.Context, res, rel, sub string) error {
	if s.failWrite {
		return errors.New("spicedb write boom")
	}
	return s.Mock.WriteRelationship(ctx, res, rel, sub)
}

// orgMux builds a registry mux with the given directory + spicedb client, gated
// open (AllowAll) so tests need no control-plane header.
func orgMux(s registry.Store, sdb spicedb.Client, dir orgdirectory.Directory) http.Handler {
	return buildMux(s, sdb, registrant.NewSpiffeVerifier(&spiffe.MockSVIDValidator{}), controlplane.AllowAll(), dir)
}

func doReq(t *testing.T, mux http.Handler, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var rdr *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, rdr)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func TestCreateOrg_HappyPath(t *testing.T) {
	s := newStore()
	sdb := spicedb.NewMock()
	dir := newFakeDirectory()
	mux := orgMux(s, sdb, dir)

	rec := doReq(t, mux, "POST", "/v1/orgs", map[string]string{"orgId": "acme", "name": "Acme", "adminUser": "alice"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	// Store record written.
	if _, ok, _ := s.GetOrg(context.Background(), "acme"); !ok {
		t.Fatal("store record not written")
	}
	// SpiceDB admin tuple written.
	if !sdb.HasRelationship("organization:acme", "admin", "user:alice") {
		t.Fatal("admin tuple not written")
	}
	// Keycloak org + first-admin membership.
	if !dir.hasCall("CreateOrg:acme") || !dir.hasCall("AddMember:acme/alice") {
		t.Fatalf("directory calls = %v", dir.calls)
	}
}

func TestCreateOrg_Idempotent(t *testing.T) {
	s := newStore()
	sdb := spicedb.NewMock()
	dir := newFakeDirectory()
	mux := orgMux(s, sdb, dir)
	doReq(t, mux, "POST", "/v1/orgs", map[string]string{"orgId": "acme", "adminUser": "alice"})
	rec := doReq(t, mux, "POST", "/v1/orgs", map[string]string{"orgId": "acme", "adminUser": "alice"})
	// A converging retry succeeds (200 rather than 201) and does not error.
	if rec.Code != http.StatusOK {
		t.Fatalf("second create status = %d, want 200", rec.Code)
	}
}

func TestCreateOrg_UnwindOnAddMemberFailure(t *testing.T) {
	s := newStore()
	sdb := spicedb.NewMock()
	dir := newFakeDirectory()
	dir.failAddMember = errors.New("keycloak add member failed")
	mux := orgMux(s, sdb, dir)

	rec := doReq(t, mux, "POST", "/v1/orgs", map[string]string{"orgId": "acme", "adminUser": "alice"})
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	// Cleanup: store record removed, admin tuple removed, keycloak org removed.
	if _, ok, _ := s.GetOrg(context.Background(), "acme"); ok {
		t.Error("store record should have been unwound")
	}
	if sdb.HasRelationship("organization:acme", "admin", "user:alice") {
		t.Error("admin tuple should have been unwound")
	}
	if !dir.hasCall("DeleteOrg:acme") {
		t.Errorf("keycloak org should have been unwound; calls=%v", dir.calls)
	}
}

func TestCreateOrg_ValidatesSlugAndAdmin(t *testing.T) {
	mux := orgMux(newStore(), spicedb.NewMock(), newFakeDirectory())
	for _, tc := range []struct{ orgID, admin string }{
		{"Acme", "alice"},    // uppercase not a DNS label
		{"-acme", "alice"},   // leading hyphen
		{"acme", ""},         // empty admin
		{"acme", "bad user"}, // invalid userId charset
	} {
		rec := doReq(t, mux, "POST", "/v1/orgs", map[string]string{"orgId": tc.orgID, "adminUser": tc.admin})
		if rec.Code != http.StatusBadRequest {
			t.Errorf("orgId=%q admin=%q: status=%d, want 400", tc.orgID, tc.admin, rec.Code)
		}
	}
}

func TestCreateOrg_DirectoryDisabled500(t *testing.T) {
	// The production disabledDirectory (no ORG_ADMIN_CLIENT_SECRET) makes org
	// mutations fail loudly rather than half-writing.
	mux := orgMux(newStore(), spicedb.NewMock(), disabledDirectory{})
	rec := doReq(t, mux, "POST", "/v1/orgs", map[string]string{"orgId": "acme", "adminUser": "alice"})
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}

func TestAddMember_DualWriteAndRoles(t *testing.T) {
	s := newStore()
	sdb := spicedb.NewMock()
	dir := newFakeDirectory()
	mux := orgMux(s, sdb, dir)
	doReq(t, mux, "POST", "/v1/orgs", map[string]string{"orgId": "acme", "adminUser": "alice"})

	// default role = member
	rec := doReq(t, mux, "POST", "/v1/orgs/acme/members", map[string]string{"user": "viewer"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("add member status = %d", rec.Code)
	}
	if !sdb.HasRelationship("organization:acme", "member", "user:viewer") {
		t.Fatal("member tuple not written")
	}
	if !dir.hasCall("AddMember:acme/viewer") {
		t.Fatal("keycloak membership not written")
	}

	// explicit admin role
	doReq(t, mux, "POST", "/v1/orgs/acme/members", map[string]string{"user": "bob", "role": "admin"})
	if !sdb.HasRelationship("organization:acme", "admin", "user:bob") {
		t.Fatal("admin tuple not written")
	}
}

func TestAddMember_UnwindOnTupleFailure(t *testing.T) {
	s := newStore()
	fs := &flakySpice{Mock: spicedb.NewMock()}
	dir := newFakeDirectory()
	mux := orgMux(s, fs, dir)
	// Create org while writes still work.
	doReq(t, mux, "POST", "/v1/orgs", map[string]string{"orgId": "acme", "adminUser": "alice"})
	// Now make the member tuple write fail.
	fs.failWrite = true
	rec := doReq(t, mux, "POST", "/v1/orgs/acme/members", map[string]string{"user": "viewer"})
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	// Keycloak membership must be unwound.
	if !dir.hasCall("RemoveMember:acme/viewer") {
		t.Errorf("keycloak membership should have been unwound; calls=%v", dir.calls)
	}
}

func TestSetRole_SwapsTuples(t *testing.T) {
	s := newStore()
	sdb := spicedb.NewMock()
	dir := newFakeDirectory()
	mux := orgMux(s, sdb, dir)
	doReq(t, mux, "POST", "/v1/orgs", map[string]string{"orgId": "acme", "adminUser": "alice"})
	doReq(t, mux, "POST", "/v1/orgs/acme/members", map[string]string{"user": "viewer"}) // member

	rec := doReq(t, mux, "PATCH", "/v1/orgs/acme/members/viewer", map[string]string{"role": "admin"})
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
	if !sdb.HasRelationship("organization:acme", "admin", "user:viewer") {
		t.Fatal("admin tuple not written")
	}
	if sdb.HasRelationship("organization:acme", "member", "user:viewer") {
		t.Fatal("stale member tuple not removed")
	}
}

func TestRemoveMember_DropsTuplesFirst(t *testing.T) {
	s := newStore()
	sdb := spicedb.NewMock()
	dir := newFakeDirectory()
	mux := orgMux(s, sdb, dir)
	doReq(t, mux, "POST", "/v1/orgs", map[string]string{"orgId": "acme", "adminUser": "alice"})
	doReq(t, mux, "POST", "/v1/orgs/acme/members", map[string]string{"user": "viewer", "role": "admin"})

	rec := doReq(t, mux, "DELETE", "/v1/orgs/acme/members/viewer", nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
	if sdb.HasRelationship("organization:acme", "member", "user:viewer") ||
		sdb.HasRelationship("organization:acme", "admin", "user:viewer") {
		t.Fatal("member/admin tuples should be gone")
	}
	if !dir.hasCall("RemoveMember:acme/viewer") {
		t.Fatal("keycloak membership not removed")
	}
}

func TestSetRole_NonMemberIs404(t *testing.T) {
	s := newStore()
	sdb := spicedb.NewMock()
	dir := newFakeDirectory()
	mux := orgMux(s, sdb, dir)
	doReq(t, mux, "POST", "/v1/orgs", map[string]string{"orgId": "acme", "adminUser": "alice"})
	// ghost is not a member of acme.
	rec := doReq(t, mux, "PATCH", "/v1/orgs/acme/members/ghost", map[string]string{"role": "admin"})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("role change for a non-member = %d, want 404", rec.Code)
	}
	if sdb.HasRelationship("organization:acme", "admin", "user:ghost") {
		t.Fatal("no tuple should be written for a non-member")
	}
}

func TestDeleteOrg_FailsClosedWhenMembersUnreadable(t *testing.T) {
	s := newStore()
	sdb := spicedb.NewMock()
	dir := newFakeDirectory()
	mux := orgMux(s, sdb, dir)
	doReq(t, mux, "POST", "/v1/orgs", map[string]string{"orgId": "acme", "adminUser": "alice"})
	// Member list unreadable ⇒ delete must fail closed (else tuples orphan).
	dir.failListMembers = errors.New("keycloak down")
	rec := doReq(t, mux, "DELETE", "/v1/orgs/acme", nil)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if _, ok, _ := s.GetOrg(context.Background(), "acme"); !ok {
		t.Fatal("store record must NOT be deleted when membership cleanup could not run")
	}
}

func TestListAndGetOrgs(t *testing.T) {
	s := newStore()
	sdb := spicedb.NewMock()
	dir := newFakeDirectory()
	mux := orgMux(s, sdb, dir)
	doReq(t, mux, "POST", "/v1/orgs", map[string]string{"orgId": "acme", "adminUser": "alice"})
	doReq(t, mux, "POST", "/v1/orgs", map[string]string{"orgId": "globex", "adminUser": "bob"})

	// List all.
	rec := doReq(t, mux, "GET", "/v1/orgs", nil)
	var all []registry.Organization
	json.Unmarshal(rec.Body.Bytes(), &all)
	if len(all) != 2 {
		t.Fatalf("list all = %d orgs, want 2", len(all))
	}

	// List by member (alice is admin/member of acme only).
	rec = doReq(t, mux, "GET", "/v1/orgs?member=alice", nil)
	var mine []registry.Organization
	json.Unmarshal(rec.Body.Bytes(), &mine)
	if len(mine) != 1 || mine[0].OrgID != "acme" {
		t.Fatalf("list by member = %+v, want [acme]", mine)
	}

	// Get one; unknown 404.
	if rec := doReq(t, mux, "GET", "/v1/orgs/acme", nil); rec.Code != http.StatusOK {
		t.Fatalf("get acme = %d", rec.Code)
	}
	if rec := doReq(t, mux, "GET", "/v1/orgs/nope", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("get unknown = %d, want 404", rec.Code)
	}
}

func TestPatchOrgDisposition(t *testing.T) {
	s := newStore()
	mux := orgMux(s, spicedb.NewMock(), newFakeDirectory())
	doReq(t, mux, "POST", "/v1/orgs", map[string]string{"orgId": "acme", "adminUser": "alice"})

	rec := doReq(t, mux, "PATCH", "/v1/orgs/acme", map[string]string{"removalDisposition": "auto-revoke"})
	if rec.Code != http.StatusOK {
		t.Fatalf("patch status = %d", rec.Code)
	}
	org, _, _ := s.GetOrg(context.Background(), "acme")
	if org.RemovalDisposition != registry.RemovalAutoRevoke {
		t.Fatalf("disposition = %q, want auto-revoke", org.RemovalDisposition)
	}
	// Invalid disposition rejected.
	if rec := doReq(t, mux, "PATCH", "/v1/orgs/acme", map[string]string{"removalDisposition": "nuke"}); rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid disposition = %d, want 400", rec.Code)
	}
}

// --- Self-serve signup guards (Phase 7) -------------------------------------

// selfServe drives the registry's create as the orchestrator's /v1/signup/org
// does: ?selfServe=1 with the caller stamped as adminUser.
func selfServeCreate(t *testing.T, mux http.Handler, orgID, admin string) *httptest.ResponseRecorder {
	t.Helper()
	return doReq(t, mux, "POST", "/v1/orgs?selfServe=1", map[string]string{"orgId": orgID, "adminUser": admin})
}

func TestSelfServeCreate_HappyPath(t *testing.T) {
	s := newStore()
	sdb := spicedb.NewMock()
	dir := newFakeDirectory()
	mux := orgMux(s, sdb, dir)

	rec := selfServeCreate(t, mux, "acme", "alice")
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	// The caller is the first admin (never a body field).
	if !sdb.HasRelationship("organization:acme", "admin", "user:alice") {
		t.Fatal("caller should be the first admin")
	}
	if !dir.hasCall("CreateOrg:acme") || !dir.hasCall("AddMember:acme/alice") {
		t.Fatalf("directory calls = %v", dir.calls)
	}
}

func TestSelfServeCreate_ReservedSlugRejected(t *testing.T) {
	mux := orgMux(newStore(), spicedb.NewMock(), newFakeDirectory())
	for _, slug := range []string{"www", "api", "auth", "admin", "mobile", "docs"} {
		rec := selfServeCreate(t, mux, slug, "alice")
		if rec.Code != http.StatusBadRequest {
			t.Errorf("reserved slug %q: status=%d, want 400", slug, rec.Code)
		}
	}
	// Reserved is enforced for the platform-admin path too (no selfServe param).
	if rec := doReq(t, mux, "POST", "/v1/orgs", map[string]string{"orgId": "api", "adminUser": "alice"}); rec.Code != http.StatusBadRequest {
		t.Errorf("reserved slug on admin path: status=%d, want 400", rec.Code)
	}
}

func TestSelfServeCreate_InvalidSlugRejected(t *testing.T) {
	mux := orgMux(newStore(), spicedb.NewMock(), newFakeDirectory())
	// A PROVIDED-but-malformed slug is rejected. (An EMPTY orgId is NOT invalid —
	// it's the auto-GUID path, covered in TestSelfServeCreate_AutoGeneratesGuid.)
	for _, slug := range []string{"Acme", "-acme", "acme-", "a_b"} {
		if rec := selfServeCreate(t, mux, slug, "alice"); rec.Code != http.StatusBadRequest {
			t.Errorf("invalid slug %q: status=%d, want 400", slug, rec.Code)
		}
	}
}

// TestSelfServeCreate_AutoGeneratesGuid pins the new contract: user-facing
// creation supplies only a display Name and omits orgId, so the registry mints an
// opaque GUID id. The Name is what the UI shows; with no slug to fall back to it
// is required.
func TestSelfServeCreate_AutoGeneratesGuid(t *testing.T) {
	s := newStore()
	sdb := spicedb.NewMock()
	dir := newFakeDirectory()
	mux := orgMux(s, sdb, dir)

	rec := doReq(t, mux, "POST", "/v1/orgs?selfServe=1", map[string]string{"name": "Acme Inc.", "adminUser": "alice"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	orgs, err := s.ListOrgs(context.Background())
	if err != nil || len(orgs) != 1 {
		t.Fatalf("want exactly 1 org (err=%v), got %d", err, len(orgs))
	}
	got := orgs[0]
	if got.Name != "Acme Inc." {
		t.Errorf("name = %q, want %q", got.Name, "Acme Inc.")
	}
	if _, perr := uuid.Parse(got.OrgID); perr != nil {
		t.Errorf("orgId %q is not a generated GUID: %v", got.OrgID, perr)
	}
	// The admin tuple + directory calls key on the GENERATED id.
	if !sdb.HasRelationship("organization:"+got.OrgID, "admin", "user:alice") {
		t.Error("caller should be admin of the generated-id org")
	}

	// No orgId AND no name → 400 (a GUID org has no other human-readable label).
	if rec := doReq(t, mux, "POST", "/v1/orgs?selfServe=1", map[string]string{"adminUser": "bob"}); rec.Code != http.StatusBadRequest {
		t.Errorf("no orgId + no name: status=%d, want 400", rec.Code)
	}
}

func TestSelfServeCreate_SecondOrgRejected(t *testing.T) {
	s := newStore()
	sdb := spicedb.NewMock()
	dir := newFakeDirectory()
	mux := orgMux(s, sdb, dir)

	if rec := selfServeCreate(t, mux, "acme", "alice"); rec.Code != http.StatusCreated {
		t.Fatalf("first org status = %d", rec.Code)
	}
	// alice already belongs to acme — a second self-serve org is refused (409),
	// and nothing is written for it.
	rec := selfServeCreate(t, mux, "globex", "alice")
	if rec.Code != http.StatusConflict {
		t.Fatalf("second org status = %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
	if _, ok, _ := s.GetOrg(context.Background(), "globex"); ok {
		t.Fatal("second org must not be created")
	}
	if dir.hasCall("CreateOrg:globex") {
		t.Fatalf("keycloak org must not be created; calls=%v", dir.calls)
	}
}

// ── Global org cap (signup-lifecycle Phase 2) ───────────────────────────────
// MAX_ORGS is read from the env inside buildMux, so these set it with t.Setenv
// before building the mux. 0/unset = unlimited; the platform-admin path bypasses.

func TestMaxOrgs_SelfServeAtCapRejected(t *testing.T) {
	t.Setenv("MAX_ORGS", "1")
	s := newStore()
	sdb := spicedb.NewMock()
	dir := newFakeDirectory()
	mux := orgMux(s, sdb, dir)

	// Seed one org via the platform-admin path (not gated) → count is now 1 == cap.
	if rec := doReq(t, mux, "POST", "/v1/orgs", map[string]string{"orgId": "acme", "adminUser": "alice"}); rec.Code != http.StatusCreated {
		t.Fatalf("seed admin create status = %d", rec.Code)
	}
	// A brand-new user's self-serve create is refused with a friendly 403 JSON.
	rec := selfServeCreate(t, mux, "globex", "bob")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("self-serve at cap status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("response body is not JSON: %v (%s)", err, rec.Body.String())
	}
	if !strings.Contains(body.Error, "capacity") {
		t.Fatalf("error message = %q, want a capacity message", body.Error)
	}
	// Nothing was written for the rejected org.
	if _, ok, _ := s.GetOrg(context.Background(), "globex"); ok {
		t.Fatal("over-cap org must not be created")
	}
	if dir.hasCall("CreateOrg:globex") {
		t.Fatalf("keycloak org must not be created; calls=%v", dir.calls)
	}
}

func TestMaxOrgs_SelfServeUnderCapAllowed(t *testing.T) {
	t.Setenv("MAX_ORGS", "5")
	s := newStore()
	mux := orgMux(s, spicedb.NewMock(), newFakeDirectory())

	// Zero existing orgs, cap 5 → the first self-serve create succeeds.
	if rec := selfServeCreate(t, mux, "acme", "alice"); rec.Code != http.StatusCreated {
		t.Fatalf("under-cap self-serve status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
}

func TestMaxOrgs_UnsetUnlimited(t *testing.T) {
	t.Setenv("MAX_ORGS", "") // explicit unset ⇒ unlimited
	s := newStore()
	mux := orgMux(s, spicedb.NewMock(), newFakeDirectory())

	// Seed one org, then a different user's self-serve create still succeeds —
	// there is no ceiling.
	if rec := doReq(t, mux, "POST", "/v1/orgs", map[string]string{"orgId": "acme", "adminUser": "alice"}); rec.Code != http.StatusCreated {
		t.Fatalf("seed create status = %d", rec.Code)
	}
	if rec := selfServeCreate(t, mux, "globex", "bob"); rec.Code != http.StatusCreated {
		t.Fatalf("unlimited self-serve status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
}

func TestMaxOrgs_AdminPathBypassesAtCap(t *testing.T) {
	t.Setenv("MAX_ORGS", "1")
	s := newStore()
	mux := orgMux(s, spicedb.NewMock(), newFakeDirectory())

	// One org already exists (count == cap).
	if rec := doReq(t, mux, "POST", "/v1/orgs", map[string]string{"orgId": "acme", "adminUser": "alice"}); rec.Code != http.StatusCreated {
		t.Fatalf("first admin create status = %d", rec.Code)
	}
	// The platform-admin path (no selfServe) creates a SECOND org past the cap.
	if rec := doReq(t, mux, "POST", "/v1/orgs", map[string]string{"orgId": "globex", "adminUser": "carol"}); rec.Code != http.StatusCreated {
		t.Fatalf("admin bypass status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	if _, ok, _ := s.GetOrg(context.Background(), "globex"); !ok {
		t.Fatal("admin-created org past the cap should exist")
	}
}

func TestSelfServeCreate_NoHijackOfExistingOrg(t *testing.T) {
	s := newStore()
	sdb := spicedb.NewMock()
	dir := newFakeDirectory()
	mux := orgMux(s, sdb, dir)

	// alice owns acme (platform-admin onboarding path).
	if rec := doReq(t, mux, "POST", "/v1/orgs", map[string]string{"orgId": "acme", "adminUser": "alice"}); rec.Code != http.StatusCreated {
		t.Fatalf("seed create status = %d", rec.Code)
	}
	// mallory tries to self-serve the SAME slug — must be refused (409), never
	// added as an admin of alice's org.
	rec := selfServeCreate(t, mux, "acme", "mallory")
	if rec.Code != http.StatusConflict {
		t.Fatalf("hijack attempt status = %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
	if sdb.HasRelationship("organization:acme", "admin", "user:mallory") {
		t.Fatal("hijack: mallory must not become an admin of acme")
	}
	if dir.hasCall("AddMember:acme/mallory") {
		t.Fatalf("hijack: mallory must not be added to acme; calls=%v", dir.calls)
	}
}

func TestAdminCreate_AllowsExistingSlugConverge(t *testing.T) {
	// The platform-admin path (no selfServe) keeps its idempotent
	// add-caller-as-admin behaviour — only self-serve refuses an existing slug.
	s := newStore()
	sdb := spicedb.NewMock()
	dir := newFakeDirectory()
	mux := orgMux(s, sdb, dir)
	doReq(t, mux, "POST", "/v1/orgs", map[string]string{"orgId": "acme", "adminUser": "alice"})
	rec := doReq(t, mux, "POST", "/v1/orgs", map[string]string{"orgId": "acme", "adminUser": "alice"})
	if rec.Code != http.StatusOK {
		t.Fatalf("admin re-create status = %d, want 200 (converge)", rec.Code)
	}
}

// --- Satellite redirect URIs (org-subdomains Phase 2) -----------------------

func TestCreateOrg_RegistersSatelliteRedirectURIs(t *testing.T) {
	t.Setenv("ORG_APEX_DOMAIN", "localtest.me")
	s := newStore()
	sdb := spicedb.NewMock()
	dir := newFakeDirectory()
	mux := orgMux(s, sdb, dir)

	rec := doReq(t, mux, "POST", "/v1/orgs", map[string]string{"orgId": "acme", "adminUser": "alice"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	want := "RegisterOrgRedirectURIs:https://acme.localtest.me/callback|https://acme.localtest.me/"
	if !dir.hasCall(want) {
		t.Fatalf("expected register call %q; calls=%v", want, dir.calls)
	}
}

func TestCreateOrg_UnwindsWhenRedirectURIRegistrationFails(t *testing.T) {
	t.Setenv("ORG_APEX_DOMAIN", "localtest.me")
	s := newStore()
	sdb := spicedb.NewMock()
	dir := newFakeDirectory()
	dir.failRegisterURIs = errors.New("keycloak PUT /clients failed")
	mux := orgMux(s, sdb, dir)

	rec := doReq(t, mux, "POST", "/v1/orgs", map[string]string{"orgId": "acme", "adminUser": "alice"})
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	// Steps 1–3 must be fully unwound (store, admin tuple, keycloak org + membership).
	if _, ok, _ := s.GetOrg(context.Background(), "acme"); ok {
		t.Error("store record should have been unwound")
	}
	if sdb.HasRelationship("organization:acme", "admin", "user:alice") {
		t.Error("admin tuple should have been unwound")
	}
	if !dir.hasCall("RemoveMember:acme/alice") {
		t.Errorf("first-admin membership should have been unwound; calls=%v", dir.calls)
	}
	if !dir.hasCall("DeleteOrg:acme") {
		t.Errorf("keycloak org should have been unwound; calls=%v", dir.calls)
	}
}

func TestDeleteOrg_RemovesSatelliteRedirectURIs(t *testing.T) {
	t.Setenv("ORG_APEX_DOMAIN", "localtest.me")
	s := newStore()
	sdb := spicedb.NewMock()
	dir := newFakeDirectory()
	mux := orgMux(s, sdb, dir)
	doReq(t, mux, "POST", "/v1/orgs", map[string]string{"orgId": "acme", "adminUser": "alice"})

	rec := doReq(t, mux, "DELETE", "/v1/orgs/acme", nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
	want := "RemoveOrgRedirectURIs:https://acme.localtest.me/callback|https://acme.localtest.me/"
	if !dir.hasCall(want) {
		t.Fatalf("expected remove call %q; calls=%v", want, dir.calls)
	}
}

func TestCreateAndDeleteOrg_ApexUnsetSkipsRegistration(t *testing.T) {
	// ORG_APEX_DOMAIN unset (feature off) → no redirect-URI calls at all.
	t.Setenv("ORG_APEX_DOMAIN", "")
	s := newStore()
	sdb := spicedb.NewMock()
	dir := newFakeDirectory()
	mux := orgMux(s, sdb, dir)

	doReq(t, mux, "POST", "/v1/orgs", map[string]string{"orgId": "acme", "adminUser": "alice"})
	doReq(t, mux, "DELETE", "/v1/orgs/acme", nil)
	if dir.hasCallPrefix("RegisterOrgRedirectURIs:") || dir.hasCallPrefix("RemoveOrgRedirectURIs:") {
		t.Fatalf("redirect-URI ops must be skipped when ORG_APEX_DOMAIN is unset; calls=%v", dir.calls)
	}
}

func TestDeleteOrg_CleansTuples(t *testing.T) {
	s := newStore()
	sdb := spicedb.NewMock()
	dir := newFakeDirectory()
	mux := orgMux(s, sdb, dir)
	doReq(t, mux, "POST", "/v1/orgs", map[string]string{"orgId": "acme", "adminUser": "alice"})
	doReq(t, mux, "POST", "/v1/orgs/acme/members", map[string]string{"user": "viewer"})

	rec := doReq(t, mux, "DELETE", "/v1/orgs/acme", nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
	if _, ok, _ := s.GetOrg(context.Background(), "acme"); ok {
		t.Fatal("store record should be gone")
	}
	if !dir.hasCall("DeleteOrg:acme") {
		t.Fatal("keycloak org not deleted")
	}
	if sdb.HasRelationship("organization:acme", "member", "user:viewer") {
		t.Fatal("membership tuple not cleaned")
	}
	// Deleting an unknown org is 404.
	if rec := doReq(t, mux, "DELETE", "/v1/orgs/acme", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("delete unknown = %d, want 404", rec.Code)
	}
}
