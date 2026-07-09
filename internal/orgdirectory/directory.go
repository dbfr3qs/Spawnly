// Package orgdirectory is the platform's neutral port for organization
// membership: the "who is in which org" directory. The registry's org
// control-plane handlers depend only on this interface, never on Keycloak — the
// Keycloak admin-REST adapter (subpackage orgdirectory/keycloak) implements it.
//
// Dependency direction (binding, see plans/multi-tenancy): the adapter adapts TO
// this port; the port references nothing Keycloak-specific. Swapping the identity
// backend means writing a new Directory, not touching the registry.
//
// Source-of-truth split (spec): the Directory (Keycloak) is the source of truth
// for RAW MEMBERSHIP — who belongs to an org — while SpiceDB is the source of
// truth for AUTHORIZATION (the member-vs-admin distinction). Keycloak
// Organizations has no native per-org role, and this realm's declarative user
// profile silently drops unmanaged user attributes, so ROLE IS NOT PERSISTED IN
// KEYCLOAK. The registry writes the organization:<id>#member|admin@user:<u>
// tuples that Phase 4's authorization flip reads; SetRole here only validates the
// subject and lets the caller keep the dual-write symmetric, and ListMembers
// returns Members with an empty Role (role is resolved from SpiceDB where the UI
// needs it — Phase 6). This keeps the platform honest: membership changes flow
// through THIS port + the SpiceDB writes, and org membership is never edited
// directly in the Keycloak admin console.
package orgdirectory

import "context"

// Role values for org membership. member = ordinary member; admin = org admin.
// These name the SpiceDB relations the registry writes (organization:<id>#member
// / #admin); the Directory itself does not persist them (see the package doc).
const (
	RoleMember = "member"
	RoleAdmin  = "admin"
)

// ValidRole reports whether r is one of the two allowed roles.
func ValidRole(r string) bool { return r == RoleMember || r == RoleAdmin }

// Member is one org member as the directory knows them. UserID is the Spawnly
// user id (== the Keycloak username, since the platform keys human identity on
// username — sub=username) and is therefore the SpiceDB subject id used in
// user:<UserID> tuples. Role is populated only by backends that track it; the
// Keycloak adapter leaves it empty (see the package doc).
type Member struct {
	UserID   string `json:"userId"`
	Username string `json:"username,omitempty"`
	Email    string `json:"email,omitempty"`
	Role     string `json:"role,omitempty"`
}

// Directory is the neutral organization-membership port. All methods are
// idempotent so a control-plane dual-write can be retried to convergence after a
// partial failure (a create-if-absent create, a delete-if-present delete): the
// registry's CreateOrg transaction relies on this to unwind cleanly.
//
// orgID is the org alias/slug (== registry OrgID == SpiceDB object id). user is a
// Spawnly user id (username); AddMember creates the user if unknown.
type Directory interface {
	// CreateOrg creates the organization with alias orgID and display name.
	// Idempotent: an already-existing org with the same alias is a success.
	CreateOrg(ctx context.Context, orgID, name string) error
	// DeleteOrg removes the organization. Idempotent: an absent org is a success.
	DeleteOrg(ctx context.Context, orgID string) error
	// AddMember adds user to the org, CREATING the user (with a required
	// UPDATE_PASSWORD action, no usable password) when unknown — the "invite by
	// username, temp-password flow" of the spec. Idempotent: an existing
	// membership is a success.
	AddMember(ctx context.Context, orgID, user string) error
	// RemoveMember removes user's membership. Idempotent: a non-member is a
	// success. Does NOT delete the user account (they may belong to other orgs).
	RemoveMember(ctx context.Context, orgID, user string) error
	// SetRole records user's role in the org. On the Keycloak adapter this only
	// validates that user exists (role lives in SpiceDB — see the package doc);
	// the caller writes the authoritative role tuple. Idempotent.
	SetRole(ctx context.Context, orgID, user, role string) error
	// ListMembers returns the org's members. Role is backend-dependent and empty
	// on the Keycloak adapter.
	ListMembers(ctx context.Context, orgID string) ([]Member, error)
	// ListUserOrgs returns the org aliases user is a member of (empty for an
	// unknown user — never an error, so a "which orgs am I in" read fails open to
	// "none" rather than leaking existence).
	ListUserOrgs(ctx context.Context, user string) ([]string, error)

	// RegisterOrgRedirectURIs adds redirectURI + postLogoutURI to the dashboard OIDC
	// client (a per-org satellite callback + post-logout origin). Idempotent. The
	// registry builds the URLs (platform owns the apex-domain shape); the adapter is
	// host-blind — dependency-direction rule.
	RegisterOrgRedirectURIs(ctx context.Context, redirectURI, postLogoutURI string) error
	// RemoveOrgRedirectURIs removes them. Idempotent (absent = success).
	RemoveOrgRedirectURIs(ctx context.Context, redirectURI, postLogoutURI string) error
}
