package main

import (
	"net"
	"net/http"
	"strings"
)

// reservedHostLabels are the subdomain labels that resolve to the APEX (no org),
// even though they are otherwise a valid DNS label. They mirror the registry's
// reservedOrgIDs (cmd/registry/orgs.go) — the org alias doubles as the subdomain
// slug, so an org can never claim one of these, and a request to
// <label>.<apex> for one of them is platform infrastructure (or a typo), not an
// org. Kept in sync with the registry set by hand; a drift would let a platform
// host be treated as an org here (fails toward no-org, so it degrades safely).
var reservedHostLabels = map[string]bool{
	"www": true, "api": true, "auth": true, "admin": true, "mobile": true, "docs": true,
}

// hostConfig is the dashboard's host-aware routing config, resolving each request
// Host to an org (a satellite) or the apex.
//
// apexDomain is the configured apex (e.g. "spawnly.run" / "localtest.me"). Its
// ZERO value ("") is the feature OFF switch: every Host resolves to the apex, so
// the dashboard behaves exactly as the single-origin build did (today's
// localhost:8090). Host is NEVER authorization — resolveOrg is routing/UX + a
// consistency guard; the orchestrator independently derives the org from the
// token for every authorization decision.
type hostConfig struct {
	apexDomain string // "" = single-origin mode (feature off)
	// trustForwardedHost honors the X-Forwarded-Host header (set by the AWS ALB,
	// which terminates TLS and rewrites Host). OFF by default: the local/kind flow
	// reads r.Host directly, and a browser-forged X-Forwarded-Host is ignored —
	// mirroring the proxy-header hygiene in main.go's Keycloak proxy director.
	trustForwardedHost bool
}

// requestHost returns the effective browser-facing host for the request. It
// honors X-Forwarded-Host ONLY when trustForwardedHost is set (a real upstream
// proxy is in front); otherwise it uses r.Host, so a client cannot steer org
// resolution by forging the header. A comma-joined header (proxy chain) yields
// its first (client-facing) value.
func (h hostConfig) requestHost(r *http.Request) string {
	if h.trustForwardedHost {
		if xfh := r.Header.Get("X-Forwarded-Host"); xfh != "" {
			if i := strings.IndexByte(xfh, ','); i >= 0 {
				xfh = xfh[:i]
			}
			return strings.TrimSpace(xfh)
		}
	}
	return r.Host
}

// resolveOrg maps a request Host to (org, isApex):
//   - APEX_DOMAIN unset → every host is the apex (feature off);
//   - the bare apex → apex;
//   - <label>.<apex> where label is reserved (www/api/auth/admin/mobile/docs) → apex;
//   - <slug>.<apex> where slug matches validOrgAlias → that org (isApex=false);
//   - anything else (multi-label, non-matching slug, unrelated host) → apex.
//
// It always FAILS TOWARD the apex (no org), never toward an arbitrary org: a
// malformed or unexpected host degrades to the safe single-origin behavior rather
// than being (mis)attributed to some org. The port is stripped before matching.
func (h hostConfig) resolveOrg(host string) (org string, isApex bool) {
	if h.apexDomain == "" {
		return "", true // single-origin mode: everything is the apex
	}
	host = strings.ToLower(strings.TrimSuffix(stripPort(host), "."))
	apex := strings.ToLower(strings.TrimSuffix(h.apexDomain, "."))
	if host == "" || apex == "" {
		return "", true
	}
	if host == apex {
		return "", true // bare apex
	}
	suffix := "." + apex
	if !strings.HasSuffix(host, suffix) {
		return "", true // unrelated host → fail toward no-org
	}
	label := strings.TrimSuffix(host, suffix)
	// Exactly one label between the slug and the apex: a multi-label prefix
	// (a.b.<apex>) is not an org subdomain — fail toward the apex.
	if label == "" || strings.Contains(label, ".") {
		return "", true
	}
	if reservedHostLabels[label] {
		return "", true // platform host, not an org
	}
	// The slug must be the same DNS-label shape the registry enforces for an org
	// alias, so a crafted host can never smuggle a value the org layer would reject.
	if !validOrgAlias.MatchString(label) {
		return "", true
	}
	return label, false
}

// stripPort removes a trailing :port from a host, if present. net.SplitHostPort
// errors when there is no port (or on a bare IPv6 without brackets), in which
// case the host is already port-free and returned as-is.
func stripPort(host string) string {
	if host == "" {
		return host
	}
	if h, _, err := net.SplitHostPort(host); err == nil {
		return h
	}
	return host
}
