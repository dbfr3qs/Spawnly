package spawnly

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync"
	"time"
)

// DefaultBaseURL is the address the per-agent sidecar listens on.
const DefaultBaseURL = "http://localhost:8089"

// expiryBuffer is how far before a cached token's true expiry we treat it as
// stale and refresh it, to avoid handing out a token that expires in flight.
const expiryBuffer = 5 * time.Second

// tokenResponse is the neutral wire shape returned by the sidecar's /token
// endpoint. Replicated locally so the SDK never imports a platform package.
type tokenResponse struct {
	AccessToken string `json:"access_token"`
	ExpiresIn   int    `json:"expires_in"`
}

// cachedToken is a token held in the TokenClient cache alongside its computed
// absolute expiry.
type cachedToken struct {
	token     string
	expiresAt time.Time
}

// TokenClient wraps the per-agent sidecar's /token endpoint — the platform's
// neutral token contract. It covers all three modes the sidecar exposes:
//
//   - GetToken(ctx, scope)                          -> client_credentials
//   - GetToken(ctx, scope, WithAudience("..."))     -> client_credentials with
//     an explicit audience (e.g. minting a delegation token)
//   - ExchangeToken(ctx, ExchangeArgs{...})         -> RFC 8693 token-exchange
//
// The sidecar binds :8089 only after it has fetched its SVID and
// self-registered, so the first calls at startup can hit connection-refused.
// Requests retry on connection errors / 5xx until ReadyTimeout, but fail fast
// on 4xx (a bad scope or policy denial is not a readiness problem).
//
// A TokenClient is safe for concurrent use by multiple goroutines.
type TokenClient struct {
	baseURL      string
	readyTimeout time.Duration
	retryDelay   time.Duration
	httpClient   *http.Client

	mu    sync.Mutex
	cache map[string]cachedToken
}

// TokenClientOption configures a TokenClient.
type TokenClientOption func(*TokenClient)

// WithBaseURL overrides the sidecar base URL (default DefaultBaseURL).
func WithBaseURL(baseURL string) TokenClientOption {
	return func(c *TokenClient) { c.baseURL = baseURL }
}

// WithReadyTimeout sets the deadline for the sidecar-not-ready retry loop
// (default 30s).
func WithReadyTimeout(d time.Duration) TokenClientOption {
	return func(c *TokenClient) { c.readyTimeout = d }
}

// WithRetryDelay sets the backoff between retries while the sidecar is
// unreachable (default 1s).
func WithRetryDelay(d time.Duration) TokenClientOption {
	return func(c *TokenClient) { c.retryDelay = d }
}

// WithHTTPClient overrides the http.Client used for token requests. Intended
// mainly for tests; the default is a fresh *http.Client.
func WithHTTPClient(h *http.Client) TokenClientOption {
	return func(c *TokenClient) { c.httpClient = h }
}

// NewTokenClient constructs a TokenClient. With no options it targets
// DefaultBaseURL with a 30s ready timeout and a 1s retry delay.
func NewTokenClient(opts ...TokenClientOption) *TokenClient {
	c := &TokenClient{
		baseURL:      DefaultBaseURL,
		readyTimeout: 30 * time.Second,
		retryDelay:   1 * time.Second,
		httpClient:   &http.Client{},
		cache:        make(map[string]cachedToken),
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// GetTokenOption configures a single GetToken call.
type GetTokenOption func(*getTokenConfig)

type getTokenConfig struct {
	audience string
}

// WithAudience targets a specific resource / mints a delegation token (e.g.
// WithAudience("delegation")).
func WithAudience(audience string) GetTokenOption {
	return func(c *getTokenConfig) { c.audience = audience }
}

// GetToken returns a client-credentials token for scope. Tokens are cached per
// "scope|audience" key until just before expiry (a 5s buffer). The retry/
// fail-fast semantics and ctx handling are described on TokenClient.
func (c *TokenClient) GetToken(ctx context.Context, scope string, opts ...GetTokenOption) (string, error) {
	var cfg getTokenConfig
	for _, opt := range opts {
		opt(&cfg)
	}

	cacheKey := scope + "|" + cfg.audience

	c.mu.Lock()
	if cached, ok := c.cache[cacheKey]; ok && time.Now().Before(cached.expiresAt.Add(-expiryBuffer)) {
		token := cached.token
		c.mu.Unlock()
		return token, nil
	}
	c.mu.Unlock()

	params := url.Values{}
	params.Set("scope", scope)
	if cfg.audience != "" {
		params.Set("audience", cfg.audience)
	}

	data, err := c.request(ctx, params)
	if err != nil {
		return "", err
	}

	c.mu.Lock()
	c.cache[cacheKey] = cachedToken{
		token:     data.AccessToken,
		expiresAt: time.Now().Add(time.Duration(data.ExpiresIn) * time.Second),
	}
	c.mu.Unlock()

	return data.AccessToken, nil
}

// ExchangeArgs are the (all required) arguments for an RFC 8693 token exchange.
type ExchangeArgs struct {
	// SubjectToken is the token being exchanged (e.g. a delegation token
	// received from a parent agent).
	SubjectToken string
	// Audience is the target audience for the exchanged token.
	Audience string
	// Scope is the requested scope for the exchanged token.
	Scope string
}

// ExchangeToken performs an RFC 8693 token exchange: it exchanges
// args.SubjectToken for a token scoped to args.Audience/args.Scope, with this
// agent's SVID added to the act chain by the sidecar. The result is NEVER
// cached — exchanged tokens are short-lived and request-specific. The same
// retry/fail-fast/ctx semantics as GetToken apply.
func (c *TokenClient) ExchangeToken(ctx context.Context, args ExchangeArgs) (string, error) {
	params := url.Values{}
	params.Set("subject_token", args.SubjectToken)
	params.Set("audience", args.Audience)
	params.Set("scope", args.Scope)

	data, err := c.request(ctx, params)
	if err != nil {
		return "", err
	}
	return data.AccessToken, nil
}

// request issues the GET, retrying on connection errors / 5xx until the
// readiness deadline, failing fast on any 4xx. It honors ctx cancellation
// throughout.
func (c *TokenClient) request(ctx context.Context, params url.Values) (tokenResponse, error) {
	reqURL := c.baseURL + "/token?" + params.Encode()

	deadline := time.Now().Add(c.readyTimeout)
	var lastErr error

	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return tokenResponse{}, err
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
		if err != nil {
			return tokenResponse{}, err
		}

		res, err := c.httpClient.Do(req)
		if err != nil {
			// If ctx was cancelled, surface that rather than retrying.
			if ctxErr := ctx.Err(); ctxErr != nil {
				return tokenResponse{}, ctxErr
			}
			// Connection error (sidecar not listening yet) — retry.
			lastErr = err
			if sleepErr := c.sleep(ctx); sleepErr != nil {
				return tokenResponse{}, sleepErr
			}
			continue
		}

		if res.StatusCode >= 200 && res.StatusCode < 300 {
			var data tokenResponse
			decErr := json.NewDecoder(res.Body).Decode(&data)
			res.Body.Close()
			if decErr != nil {
				return tokenResponse{}, fmt.Errorf("decode token response: %w", decErr)
			}
			return data, nil
		}

		// 4xx — a real error (bad scope / policy denial), not readiness. Fail fast.
		if res.StatusCode >= 400 && res.StatusCode < 500 {
			body, _ := io.ReadAll(res.Body)
			res.Body.Close()
			return tokenResponse{}, fmt.Errorf("token request failed: %d %s", res.StatusCode, string(body))
		}

		// 5xx (or other non-2xx) — transient; retry until the deadline.
		res.Body.Close()
		lastErr = fmt.Errorf("token request failed: %d", res.StatusCode)
		if sleepErr := c.sleep(ctx); sleepErr != nil {
			return tokenResponse{}, sleepErr
		}
	}

	return tokenResponse{}, fmt.Errorf("sidecar /token unreachable after %s: %w", c.readyTimeout, lastErr)
}

// sleep waits retryDelay, returning early with ctx.Err() if ctx is done.
func (c *TokenClient) sleep(ctx context.Context) error {
	t := time.NewTimer(c.retryDelay)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
