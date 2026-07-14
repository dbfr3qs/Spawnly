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

import (
	"context"
	"errors"
)

// ErrConflict is a neutral, backend-agnostic "the requested change collides with
// an existing record" signal an adapter may return so a control-plane handler can
// map it to a 409 instead of a blanket 500. The Keycloak adapter returns it (via
// errors.Is) when a UpdateUser email edit hits the realm's unique-email constraint
// — a routine, admin-resolvable outcome, not a server fault. Kept here (the port),
// not in the adapter, so the dependency direction holds: the registry depends on
// this package, never on Keycloak.
var ErrConflict = errors.New("orgdirectory: conflict")

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
	// Enabled reports whether the member's account may currently log in. Populated
	// by the Keycloak adapter from the members listing. Backends that don't track it
	// leave it at the zero value (false).
	Enabled bool `json:"enabled,omitempty"`
}

// User is one platform user as the directory knows them (the master-admin user
// directory, signup-lifecycle Phase 3) — a neutral, platform-shaped subset of
// the identity backend's user representation. Username is the Spawnly user id
// (== the Keycloak username / SpiceDB subject id). Enabled reports whether the
// account may log in. DisabledByOrg carries the FIRST org alias in the account's
// (possibly multi-valued) suspending-org set — empty when no org suspends it — so
// the master-admin directory can label an org suspension; the full set + the
// separate admin-ban marker are the adapter's internal state (see keycloak.go).
type User struct {
	Username      string `json:"username"`
	Email         string `json:"email,omitempty"`
	FirstName     string `json:"firstName,omitempty"`
	LastName      string `json:"lastName,omitempty"`
	Enabled       bool   `json:"enabled"`
	DisabledByOrg string `json:"disabledByOrg,omitempty"`
}

// UserUpdate is a partial edit of a user's profile fields (master-admin edit-user,
// account-lifecycle follow-up). Each field is a POINTER so the adapter can tell
// "set this field to the given value" (non-nil) from "leave this field unchanged"
// (nil) — a nil field is never written, so an edit that only changes the email
// does not blank the names, and vice-versa. This is deliberately a neutral,
// platform-shaped struct: NO identity-backend types leak through this port
// (dependency-direction rule). The editable set is intentionally narrow — profile
// fields only. It does NOT carry enabled/roles/attributes, so edit-user can never
// be used to lift a suspension, grant a role, or touch the disabledByOrg /
// disabledByAdmin lifecycle attributes (those flow only through Disable/EnableUser).
type UserUpdate struct {
	Email     *string
	FirstName *string
	LastName  *string
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

	// ── User lifecycle (master-admin surface, signup-lifecycle Phase 3) ────────

	// ListUsers returns every user the directory knows about (paginated
	// internally). Powers the master-admin user directory.
	ListUsers(ctx context.Context) ([]User, error)
	// DisableUser blocks the user from logging in (enabled=false) AND revokes
	// their active SSO sessions, so a disable takes effect immediately rather than
	// only on the next login. When byOrg != "" it ADDS byOrg to the multi-valued
	// disabledByOrg attribute (the SET of orgs currently suspending the account,
	// dedup); when byOrg == "" it is a direct admin ban and sets a separate
	// disabledByAdmin marker instead. The two reasons are tracked independently so
	// neither clobbers the other. Read-modify-write preserves the user's other
	// attributes. Idempotent; an unknown user is a no-op success.
	DisableUser(ctx context.Context, username, byOrg string) error
	// EnableUser lifts ONE suspension reason and re-enables login only when NONE
	// remain. When onlyIfDisabledByOrg != "" it REMOVES that org from the
	// disabledByOrg set (a no-op leaving the account untouched if that org wasn't
	// suspending it); when onlyIfDisabledByOrg == "" it clears the direct-admin ban
	// marker. In both cases the account is re-enabled ONLY if it is no longer
	// admin-banned AND the disabledByOrg set is now empty — so an org lifting its
	// suspension while another org (or the platform admin) still holds one keeps the
	// account disabled. Returns whether anything changed. Read-modify-write
	// preserves other attributes. Idempotent; an unknown user returns (false, nil).
	EnableUser(ctx context.Context, username, onlyIfDisabledByOrg string) (bool, error)
	// DeleteUser hard-deletes the user account. Idempotent (an absent user is a
	// success). Callers settle the user's org agents + memberships FIRST (the
	// registry choke point) — this only removes the identity.
	DeleteUser(ctx context.Context, username string) error
	// UpdateUser applies a PARTIAL edit of the user's profile (email/first/last
	// name). Only the non-nil fields of attrs are written; every other field of the
	// user — including the enabled state and the disabledByOrg/disabledByAdmin
	// lifecycle attributes — is left exactly as-is (read-modify-write). Setting a
	// non-empty email pins the account email-verified (this is an admin giving a
	// user a known address — the same trust stance createUser takes); clearing the
	// email (empty string) clears the verified flag. Idempotent; an unknown user is
	// a no-op success. Serialized against Disable/EnableUser on the same user so a
	// concurrent lifecycle mutation can't race the edit.
	UpdateUser(ctx context.Context, username string, attrs UserUpdate) error
}
