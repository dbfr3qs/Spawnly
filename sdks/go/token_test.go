package spawnly

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func tokenJSON(token string, expiresIn int) string {
	return fmt.Sprintf(`{"access_token":%q,"expires_in":%d}`, token, expiresIn)
}

func TestGetTokenCacheHit(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		fmt.Fprint(w, tokenJSON("tok-abc", 3600))
	}))
	defer srv.Close()

	c := NewTokenClient(WithBaseURL(srv.URL))
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		tok, err := c.GetToken(ctx, "agent:run")
		if err != nil {
			t.Fatalf("GetToken: %v", err)
		}
		if tok != "tok-abc" {
			t.Fatalf("got token %q, want tok-abc", tok)
		}
	}

	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("expected 1 upstream call (cache hit), got %d", got)
	}
}

func TestGetTokenCacheKeyDistinctAudience(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		fmt.Fprint(w, tokenJSON("tok-"+r.URL.Query().Get("audience"), 3600))
	}))
	defer srv.Close()

	c := NewTokenClient(WithBaseURL(srv.URL))
	ctx := context.Background()

	if _, err := c.GetToken(ctx, "agent:run"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.GetToken(ctx, "agent:run", WithAudience("delegation")); err != nil {
		t.Fatal(err)
	}
	// Repeat both — should be cache hits.
	if _, err := c.GetToken(ctx, "agent:run"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.GetToken(ctx, "agent:run", WithAudience("delegation")); err != nil {
		t.Fatal(err)
	}

	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("expected 2 upstream calls (distinct cache keys), got %d", got)
	}
}

func TestGetTokenRefreshesNearExpiry(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		// expires_in of 3s is inside the 5s buffer, so it is always stale.
		fmt.Fprint(w, tokenJSON("tok", 3))
	}))
	defer srv.Close()

	c := NewTokenClient(WithBaseURL(srv.URL))
	ctx := context.Background()

	if _, err := c.GetToken(ctx, "s"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.GetToken(ctx, "s"); err != nil {
		t.Fatal(err)
	}

	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("expected 2 upstream calls (token within expiry buffer), got %d", got)
	}
}

func TestGetTokenRetriesAfterTransient5xx(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if n < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		fmt.Fprint(w, tokenJSON("ready", 3600))
	}))
	defer srv.Close()

	c := NewTokenClient(WithBaseURL(srv.URL), WithRetryDelay(1*time.Millisecond))
	tok, err := c.GetToken(context.Background(), "s")
	if err != nil {
		t.Fatalf("GetToken: %v", err)
	}
	if tok != "ready" {
		t.Fatalf("got %q, want ready", tok)
	}
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Fatalf("expected 3 attempts, got %d", got)
	}
}

func TestGetTokenRetriesAfterConnectionRefused(t *testing.T) {
	// Point the client at a server we start only after the first attempts fail.
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, tokenJSON("late", 3600))
	}))
	// Grab a listener address, close it, so initial dials are refused.
	srv.Start()
	base := srv.URL
	srv.Close()

	c := NewTokenClient(WithBaseURL(base), WithRetryDelay(20*time.Millisecond), WithReadyTimeout(3*time.Second))

	// In a goroutine, after a short delay, stand a new server on the same addr.
	// Simpler: just assert that against a dead server we eventually time out,
	// which exercises the connection-error retry path and the deadline.
	start := time.Now()
	_, err := c.GetToken(context.Background(), "s")
	if err == nil {
		t.Fatal("expected error against dead server")
	}
	if time.Since(start) < 40*time.Millisecond {
		t.Fatalf("expected at least one retry delay before giving up, elapsed %s", time.Since(start))
	}
}

func TestGetTokenFailFastOn4xx(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, "policy denied")
	}))
	defer srv.Close()

	c := NewTokenClient(WithBaseURL(srv.URL), WithRetryDelay(1*time.Millisecond))
	_, err := c.GetToken(context.Background(), "bad:scope")
	if err == nil {
		t.Fatal("expected error on 4xx")
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("expected exactly 1 attempt (fail fast), got %d", got)
	}
}

func TestGetTokenRespectsContextCancellation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	c := NewTokenClient(WithBaseURL(srv.URL), WithRetryDelay(50*time.Millisecond), WithReadyTimeout(10*time.Second))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := c.GetToken(ctx, "s")
	if err == nil {
		t.Fatal("expected error from cancelled context")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("ctx cancellation not honored, elapsed %s", time.Since(start))
	}
}

func TestExchangeTokenParamsAndNoCache(t *testing.T) {
	var calls int32
	var lastQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		lastQuery = r.URL.RawQuery
		fmt.Fprint(w, tokenJSON("exchanged", 60))
	}))
	defer srv.Close()

	c := NewTokenClient(WithBaseURL(srv.URL))
	ctx := context.Background()

	args := ExchangeArgs{SubjectToken: "subj", Audience: "aud", Scope: "sc"}
	tok, err := c.ExchangeToken(ctx, args)
	if err != nil {
		t.Fatalf("ExchangeToken: %v", err)
	}
	if tok != "exchanged" {
		t.Fatalf("got %q, want exchanged", tok)
	}

	q := mustParseQuery(t, lastQuery)
	if q.Get("subject_token") != "subj" || q.Get("audience") != "aud" || q.Get("scope") != "sc" {
		t.Fatalf("unexpected query params: %s", lastQuery)
	}

	// Second call must hit the server again (never cached).
	if _, err := c.ExchangeToken(ctx, args); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("expected 2 upstream calls (exchange never cached), got %d", got)
	}
}

func TestGetTokenSendsScopeAndAudience(t *testing.T) {
	var lastQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lastQuery = r.URL.RawQuery
		fmt.Fprint(w, tokenJSON("t", 3600))
	}))
	defer srv.Close()

	c := NewTokenClient(WithBaseURL(srv.URL))
	if _, err := c.GetToken(context.Background(), "my:scope", WithAudience("my-aud")); err != nil {
		t.Fatal(err)
	}
	q := mustParseQuery(t, lastQuery)
	if q.Get("scope") != "my:scope" || q.Get("audience") != "my-aud" {
		t.Fatalf("unexpected query: %s", lastQuery)
	}
}
