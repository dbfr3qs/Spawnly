package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sort"
	"sync"
	"testing"

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

	failCreateOrg    error
	failAddMember    error
	failRemoveMember error
	failDeleteOrg    error
	failSetRole      error
	failListMembers  error
}

func newFakeDirectory() *fakeDirectory {
	return &fakeDirectory{orgs: map[string]bool{}, members: map[string]map[string]bool{}}
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
		out = append(out, orgdirectory.Member{UserID: u, Username: u})
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
