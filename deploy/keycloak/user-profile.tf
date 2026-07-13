# ===========================================================================
# Declarative user profile for the `spawnly` realm (signup-lifecycle Phase 4).
#
# WHY THIS EXISTS: the org-lifecycle disable/enable flow stamps a `disabledByOrg`
# user attribute (the org alias that suspended an account) so a later org-enable
# re-enables ONLY the members THAT org disabled, never an independently-disabled
# account. On Keycloak 24+ the declarative user profile runs with the
# unmanaged-attribute policy DISABLED by default, so an admin-REST PUT carrying an
# UNDECLARED attribute is SILENTLY DROPPED — `disabledByOrg` would never persist
# and org-enable's no-clobber guard would break (see the breadcrumb at
# internal/orgdirectory/keycloak/keycloak.go `disabledByOrgAttr`).
#
# LEAST-PRIVILEGE CHOICE: rather than opening the realm's unmanaged-attribute
# policy (which would let ANY admin-set attribute persist), we DECLARE exactly one
# extra managed attribute, `disabledByOrg`, with admin-only view/edit permissions —
# so only that single tag persists, users can never set it through the account or
# registration forms, and the unmanaged policy stays DISABLED (the default).
#
# CARE: declaring a user profile REPLACES the realm's whole profile config, so the
# four stock attributes (username, email, firstName, lastName) are replicated here
# BYTE-FOR-BYTE from the shipped default (dumped live via
# `kcadm.sh get users/profile -r spawnly`) — their validators/permissions/required
# roles are load-bearing (username length + prohibited-characters, email format,
# firstName/lastName required-for-user drive the register form). Only the
# `disabledByOrg` attribute + its `user-metadata` group membership are additive.

resource "keycloak_realm_user_profile" "spawnly" {
  realm_id = keycloak_realm.spawnly.id

  # ── Stock attributes (replicated from the Keycloak 26.4 default profile) ──────
  attribute {
    name         = "username"
    display_name = "$${username}"
    validator {
      name = "length"
      config = {
        min = 3
        max = 255
      }
    }
    validator {
      name = "username-prohibited-characters"
    }
    validator {
      name = "up-username-not-idn-homograph"
    }
    permissions {
      view = ["admin", "user"]
      edit = ["admin", "user"]
    }
  }

  attribute {
    name         = "email"
    display_name = "$${email}"
    validator {
      name = "email"
    }
    validator {
      name = "length"
      config = {
        max = 255
      }
    }
    required_for_roles = ["user"]
    permissions {
      view = ["admin", "user"]
      edit = ["admin", "user"]
    }
  }

  attribute {
    name         = "firstName"
    display_name = "$${firstName}"
    validator {
      name = "length"
      config = {
        max = 255
      }
    }
    validator {
      name = "person-name-prohibited-characters"
    }
    required_for_roles = ["user"]
    permissions {
      view = ["admin", "user"]
      edit = ["admin", "user"]
    }
  }

  attribute {
    name         = "lastName"
    display_name = "$${lastName}"
    validator {
      name = "length"
      config = {
        max = 255
      }
    }
    validator {
      name = "person-name-prohibited-characters"
    }
    required_for_roles = ["user"]
    permissions {
      view = ["admin", "user"]
      edit = ["admin", "user"]
    }
  }

  # ── Additive: the org-suspension SET (admin-only, never user-editable) ────────
  # MULTI-VALUED: a user may be suspended by several orgs at once, so disabledByOrg
  # holds the SET of suspending org aliases. multi_valued=true is load-bearing — a
  # single-valued managed attribute would TRUNCATE the array to one value on the
  # admin PUT, and an org-enable that removed that one value would wrongly re-enable
  # a member another org still suspends (see keycloak.go disabledByOrgAttr).
  attribute {
    name         = "disabledByOrg"
    display_name = "Disabled by organization"
    group        = "user-metadata"
    multi_valued = true
    permissions {
      view = ["admin"]
      edit = ["admin"]
    }
  }

  # ── Additive: the direct-admin ban marker (admin-only, never user-editable) ───
  # A platform ban is tracked SEPARATELY from org suspension so an org-enable that
  # empties disabledByOrg can never resurrect an admin-banned account, and a direct
  # admin-enable that clears this marker can never resurrect an org-suspended one.
  attribute {
    name         = "disabledByAdmin"
    display_name = "Disabled by platform admin"
    group        = "user-metadata"
    permissions {
      view = ["admin"]
      edit = ["admin"]
    }
  }

  group {
    name                = "user-metadata"
    display_header      = "User metadata"
    display_description = "Attributes, which refer to user metadata"
  }
}
