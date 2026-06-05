package spawnly

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func mustParseQuery(t *testing.T, raw string) url.Values {
	t.Helper()
	v, err := url.ParseQuery(raw)
	if err != nil {
		t.Fatalf("parse query %q: %v", raw, err)
	}
	return v
}

func TestTenantHeaderPresent(t *testing.T) {
	h := TenantHeader("acme")
	if got := h.Get("X-Tenant-ID"); got != "acme" {
		t.Fatalf("got %q, want acme", got)
	}
}

func TestTenantHeaderAbsent(t *testing.T) {
	h := TenantHeader("")
	if len(h) != 0 {
		t.Fatalf("expected empty header, got %v", h)
	}
	if got := h.Get("X-Tenant-ID"); got != "" {
		t.Fatalf("got %q, want empty", got)
	}
}

// tokenServer returns a test server that always returns a fixed token.
func tokenServer(t *testing.T, token string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, tokenJSON(token, 3600))
	}))
}

func TestAuthenticatedClientSetsAuthAndTenant(t *testing.T) {
	tokSrv := tokenServer(t, "secret-token")
	defer tokSrv.Close()

	var gotAuth, gotTenant, gotPath string
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotTenant = r.Header.Get("X-Tenant-ID")
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer apiSrv.Close()

	tc := NewTokenClient(WithBaseURL(tokSrv.URL))
	ac := NewAuthenticatedClient(apiSrv.URL, "agent:call", tc, WithTenantID("tenant-7"))

	res, err := ac.Get(context.Background(), "/v1/things")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	res.Body.Close()

	if gotAuth != "Bearer secret-token" {
		t.Fatalf("Authorization = %q, want Bearer secret-token", gotAuth)
	}
	if gotTenant != "tenant-7" {
		t.Fatalf("X-Tenant-ID = %q, want tenant-7", gotTenant)
	}
	if gotPath != "/v1/things" {
		t.Fatalf("path = %q, want /v1/things", gotPath)
	}
}

func TestAuthenticatedClientNoTenantHeaderWhenGlobal(t *testing.T) {
	tokSrv := tokenServer(t, "t")
	defer tokSrv.Close()

	var tenantPresent bool
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, tenantPresent = r.Header["X-Tenant-Id"]
		w.WriteHeader(http.StatusOK)
	}))
	defer apiSrv.Close()

	tc := NewTokenClient(WithBaseURL(tokSrv.URL))
	ac := NewAuthenticatedClient(apiSrv.URL, "s", tc) // no tenant

	res, err := ac.Get(context.Background(), "/x")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()

	if tenantPresent {
		t.Fatal("expected no X-Tenant-ID header for a global agent")
	}
}

func TestAuthenticatedClientResolvesRelativeAndAbsolute(t *testing.T) {
	tokSrv := tokenServer(t, "t")
	defer tokSrv.Close()

	var hitPath string
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hitPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer apiSrv.Close()

	tc := NewTokenClient(WithBaseURL(tokSrv.URL))
	// Base URL deliberately different from the absolute target server.
	ac := NewAuthenticatedClient("http://base.invalid", "s", tc)

	// Absolute URL should pass through and reach apiSrv.
	res, err := ac.Get(context.Background(), apiSrv.URL+"/absolute")
	if err != nil {
		t.Fatalf("absolute Get: %v", err)
	}
	res.Body.Close()
	if hitPath != "/absolute" {
		t.Fatalf("absolute path = %q, want /absolute", hitPath)
	}

	// Relative path resolves against the (invalid) base — confirm via resolveURL.
	got := ac.resolveURL("/rel")
	if got != "http://base.invalid/rel" {
		t.Fatalf("resolveURL relative = %q", got)
	}
	if pass := ac.resolveURL("https://other/x"); pass != "https://other/x" {
		t.Fatalf("resolveURL absolute = %q", pass)
	}
}

func TestAuthenticatedClientPostSendsBody(t *testing.T) {
	tokSrv := tokenServer(t, "t")
	defer tokSrv.Close()

	var gotBody string
	var gotMethod string
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusCreated)
	}))
	defer apiSrv.Close()

	tc := NewTokenClient(WithBaseURL(tokSrv.URL))
	ac := NewAuthenticatedClient(apiSrv.URL, "s", tc)

	res, err := ac.Post(context.Background(), "/things", strings.NewReader(`{"a":1}`))
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	res.Body.Close()

	if gotMethod != http.MethodPost {
		t.Fatalf("method = %q", gotMethod)
	}
	if gotBody != `{"a":1}` {
		t.Fatalf("body = %q", gotBody)
	}
}

func TestAuthenticatedClientPropagatesTokenError(t *testing.T) {
	// Token server always 403 -> GetToken fails fast -> Do should error.
	tokSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer tokSrv.Close()

	tc := NewTokenClient(WithBaseURL(tokSrv.URL))
	ac := NewAuthenticatedClient("http://unused.invalid", "s", tc)

	if _, err := ac.Get(context.Background(), "/x"); err == nil {
		t.Fatal("expected error when token fetch fails")
	}
}
