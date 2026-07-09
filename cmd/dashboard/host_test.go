package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestResolveOrg is the table-driven proof of host→org resolution across every
// decision-3 case: single-origin mode (APEX_DOMAIN unset), the bare apex, each
// reserved label, a valid org slug, port stripping, and every fail-toward-apex
// case (multi-label, invalid slug, unrelated host, empty). resolveOrg must NEVER
// attribute a malformed/unexpected host to an arbitrary org.
func TestResolveOrg(t *testing.T) {
	cases := []struct {
		name       string
		apex       string
		host       string
		wantOrg    string
		wantIsApex bool
	}{
		// Single-origin mode: APEX_DOMAIN unset → everything is the apex (feature off).
		{"feature off, plain host", "", "localhost:8090", "", true},
		{"feature off, subdomain-looking host", "", "acme.spawnly.run", "", true},

		// Bare apex.
		{"bare apex", "spawnly.run", "spawnly.run", "", true},
		{"bare apex with port", "spawnly.run", "spawnly.run:443", "", true},
		{"bare apex trailing dot", "spawnly.run", "spawnly.run.", "", true},
		{"bare apex mixed case", "spawnly.run", "Spawnly.RUN", "", true},

		// Valid org satellites.
		{"org satellite", "spawnly.run", "acme.spawnly.run", "acme", false},
		{"org satellite with port", "spawnly.run", "acme.spawnly.run:443", "acme", false},
		{"org satellite mixed case", "spawnly.run", "Acme.Spawnly.Run", "acme", false},
		{"hyphenated org slug", "spawnly.run", "acme-corp.spawnly.run", "acme-corp", false},
		{"local apex satellite", "localtest.me", "globex.localtest.me", "globex", false},

		// Reserved labels → apex (mirror reservedOrgIDs).
		{"reserved www", "spawnly.run", "www.spawnly.run", "", true},
		{"reserved api", "spawnly.run", "api.spawnly.run", "", true},
		{"reserved auth", "spawnly.run", "auth.spawnly.run", "", true},
		{"reserved admin", "spawnly.run", "admin.spawnly.run", "", true},
		{"reserved mobile", "spawnly.run", "mobile.spawnly.run", "", true},
		{"reserved docs", "spawnly.run", "docs.spawnly.run", "", true},

		// Fail toward apex: multi-label, invalid slug, unrelated, empty.
		{"multi-label prefix", "spawnly.run", "a.b.spawnly.run", "", true},
		{"invalid slug (uppercase already lowered ok) underscore", "spawnly.run", "ac_me.spawnly.run", "", true},
		{"invalid slug leading hyphen", "spawnly.run", "-acme.spawnly.run", "", true},
		{"unrelated host", "spawnly.run", "evil.com", "", true},
		{"apex as suffix of a different tld", "spawnly.run", "acme.spawnly.run.evil.com", "", true},
		{"empty host", "spawnly.run", "", "", true},
		{"host is just the apex-with-leading-dot", "spawnly.run", ".spawnly.run", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := hostConfig{apexDomain: tc.apex}
			org, isApex := h.resolveOrg(tc.host)
			if org != tc.wantOrg || isApex != tc.wantIsApex {
				t.Errorf("resolveOrg(%q) [apex=%q] = (%q,%v), want (%q,%v)", tc.host, tc.apex, org, isApex, tc.wantOrg, tc.wantIsApex)
			}
		})
	}
}

// TestRequestHost proves X-Forwarded-Host is honored ONLY when trustForwardedHost
// is set (the ALB deploy). A forged header is ignored otherwise, so a client can't
// steer org resolution; a proxy chain (comma-joined) yields the first value.
func TestRequestHost(t *testing.T) {
	newReq := func(host, xfh string) *http.Request {
		r := httptest.NewRequest("GET", "/", nil)
		r.Host = host
		if xfh != "" {
			r.Header.Set("X-Forwarded-Host", xfh)
		}
		return r
	}

	// Untrusted (default): always r.Host; the forged header is ignored.
	if got := (hostConfig{}).requestHost(newReq("acme.spawnly.run", "evil.spawnly.run")); got != "acme.spawnly.run" {
		t.Errorf("untrusted requestHost = %q, want acme.spawnly.run (X-Forwarded-Host must be ignored)", got)
	}
	// Trusted: the header wins over r.Host.
	trust := hostConfig{trustForwardedHost: true}
	if got := trust.requestHost(newReq("dashboard:8080", "acme.spawnly.run")); got != "acme.spawnly.run" {
		t.Errorf("trusted requestHost = %q, want acme.spawnly.run", got)
	}
	// Trusted but no header → r.Host.
	if got := trust.requestHost(newReq("acme.spawnly.run", "")); got != "acme.spawnly.run" {
		t.Errorf("trusted requestHost (no header) = %q, want acme.spawnly.run", got)
	}
	// Trusted, proxy chain → first value.
	if got := trust.requestHost(newReq("dashboard:8080", "acme.spawnly.run, alb.internal")); got != "acme.spawnly.run" {
		t.Errorf("trusted requestHost (chain) = %q, want acme.spawnly.run", got)
	}
}

// TestResolveOrgHonorsForwardedHost ties requestHost + resolveOrg together: with
// TRUST_FORWARDED_HOST off, a forged X-Forwarded-Host cannot make the resolver see
// a satellite; with it on, the trusted header drives resolution.
func TestResolveOrgHonorsForwardedHost(t *testing.T) {
	mk := func(host, xfh string) *http.Request {
		r := httptest.NewRequest("GET", "/", nil)
		r.Host = host
		r.Header.Set("X-Forwarded-Host", xfh)
		return r
	}
	untrusted := hostConfig{apexDomain: "spawnly.run"}
	if org, isApex := untrusted.resolveOrg(untrusted.requestHost(mk("spawnly.run", "acme.spawnly.run"))); org != "" || !isApex {
		t.Errorf("untrusted forged XFH resolved to (%q,%v), want apex", org, isApex)
	}
	trusted := hostConfig{apexDomain: "spawnly.run", trustForwardedHost: true}
	if org, isApex := trusted.resolveOrg(trusted.requestHost(mk("dashboard:8080", "acme.spawnly.run"))); org != "acme" || isApex {
		t.Errorf("trusted XFH resolved to (%q,%v), want (acme,false)", org, isApex)
	}
}
