package mobilegateway

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/lestrrat-go/jwx/v2/jwa"
	"github.com/lestrrat-go/jwx/v2/jws"
	"github.com/lestrrat-go/jwx/v2/jwt"
)

// ErrDeviceUnregistered is returned by a transport Send when the push provider
// reports the device token is no longer valid (APNs 410 / BadDeviceToken, FCM
// UNREGISTERED). The notify handler prunes the device on this signal so dead
// tokens don't accumulate.
var ErrDeviceUnregistered = errors.New("device push token is no longer registered")

// FCMAPNsTransport is the production push transport: direct FCM (HTTP v1) for
// Android and direct APNs (token-based, HTTP/2) for iOS — no third-party broker.
// Credentials come from mounted secrets, never the image (see buildFCMAPNs).
//
// NOTE: the request shaping, auth-token minting, and unregistered handling are
// unit-tested against httptest stubs, but real delivery to Apple/Google is only
// verifiable from a deployed AWS environment with real credentials (see the AWS
// runbook). It is intentionally written to be correct-by-construction.
type FCMAPNsTransport struct {
	fcm  *fcmSender
	apns *apnsSender
}

// FCMAPNsConfig holds the provider credentials, read from mounted secret files
// (never the image). serviceAccountJSON is a Google service-account key; the
// APNs fields come from an Apple developer account + the .p8 auth key.
type FCMAPNsConfig struct {
	// FCM
	ServiceAccountJSON []byte
	// APNs
	APNsKeyP8    []byte
	APNsKeyID    string
	APNsTeamID   string
	APNsBundleID string
	// APNsProduction selects api.push.apple.com (true) vs the sandbox host.
	APNsProduction bool
	// HTTPClient is shared by both senders; defaulted when nil.
	HTTPClient *http.Client
}

const (
	apnsProdURL    = "https://api.push.apple.com"
	apnsSandboxURL = "https://api.sandbox.push.apple.com"
	fcmBaseURL     = "https://fcm.googleapis.com"
)

// NewFCMAPNsTransport builds the production transport from credentials. It fails
// fast on malformed credentials so a misconfigured deployment doesn't silently
// drop every push.
func NewFCMAPNsTransport(cfg FCMAPNsConfig) (*FCMAPNsTransport, error) {
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}

	var sa struct {
		ClientEmail string `json:"client_email"`
		PrivateKey  string `json:"private_key"`
		TokenURI    string `json:"token_uri"`
		ProjectID   string `json:"project_id"`
	}
	if err := json.Unmarshal(cfg.ServiceAccountJSON, &sa); err != nil {
		return nil, fmt.Errorf("parse service account json: %w", err)
	}
	if sa.ClientEmail == "" || sa.PrivateKey == "" || sa.ProjectID == "" {
		return nil, errors.New("service account json missing client_email/private_key/project_id")
	}
	rsaKey, err := parseRSAKey([]byte(sa.PrivateKey))
	if err != nil {
		return nil, fmt.Errorf("parse service account private key: %w", err)
	}
	tokenURI := sa.TokenURI
	if tokenURI == "" {
		tokenURI = "https://oauth2.googleapis.com/token"
	}

	ecKey, err := parseECKey(cfg.APNsKeyP8)
	if err != nil {
		return nil, fmt.Errorf("parse apns .p8 key: %w", err)
	}
	if cfg.APNsKeyID == "" || cfg.APNsTeamID == "" || cfg.APNsBundleID == "" {
		return nil, errors.New("apns config missing keyId/teamId/bundleId")
	}
	apnsURL := apnsSandboxURL
	if cfg.APNsProduction {
		apnsURL = apnsProdURL
	}

	return &FCMAPNsTransport{
		fcm: &fcmSender{
			projectID: sa.ProjectID,
			tokens:    &googleTokenSource{clientEmail: sa.ClientEmail, privateKey: rsaKey, tokenURI: tokenURI, client: client},
			baseURL:   fcmBaseURL,
			client:    client,
		},
		apns: &apnsSender{
			topic:   cfg.APNsBundleID,
			signer:  &apnsSigner{keyID: cfg.APNsKeyID, teamID: cfg.APNsTeamID, key: ecKey},
			baseURL: apnsURL,
			client:  client,
		},
	}, nil
}

func (t *FCMAPNsTransport) Name() string { return "fcmapns" }

// Send routes by platform. A device with an unknown platform is a no-op error
// (it should never have been stored — registerDevice validates platform).
func (t *FCMAPNsTransport) Send(ctx context.Context, d Device, e Event) error {
	switch d.Platform {
	case PlatformAndroid:
		return t.fcm.send(ctx, d.PushToken, e)
	case PlatformIOS:
		return t.apns.send(ctx, d.PushToken, e)
	default:
		return fmt.Errorf("unknown device platform %q", d.Platform)
	}
}

// pushBody is the minimal, secret-free data both providers carry: the app uses
// consentRequestId to deep-link and re-fetches the authoritative request over
// the authenticated channel. No scopes/binding message ride the push.
func pushData(e Event) map[string]string {
	return map[string]string{
		"type":             e.Type,
		"consentRequestId": e.ConsentRequestID,
		"agentId":          e.AgentID,
	}
}

// --- FCM (Android), HTTP v1 -------------------------------------------------

type fcmSender struct {
	projectID string
	tokens    *googleTokenSource
	baseURL   string // https://fcm.googleapis.com, overridable in tests
	client    *http.Client
}

func (s *fcmSender) send(ctx context.Context, deviceToken string, e Event) error {
	access, err := s.tokens.token(ctx)
	if err != nil {
		return fmt.Errorf("fcm auth: %w", err)
	}
	payload, _ := json.Marshal(map[string]any{
		"message": map[string]any{
			"token": deviceToken,
			// Visible notification + data for deep-link. Title/body are generic
			// (no scopes/binding) — the lock screen must not leak request detail.
			"notification": map[string]string{
				"title": "Approval needed",
				"body":  "An agent is requesting your authorization.",
			},
			"data": pushData(e),
		},
	})
	url := fmt.Sprintf("%s/v1/projects/%s/messages:send", s.baseURL, s.projectID)
	req, _ := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(payload))
	req.Header.Set("Authorization", "Bearer "+access)
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		return nil
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	// FCM v1 reports a stale token as 404 NOT_FOUND / UNREGISTERED.
	if resp.StatusCode == http.StatusNotFound || bytes.Contains(body, []byte("UNREGISTERED")) {
		return ErrDeviceUnregistered
	}
	return fmt.Errorf("fcm send: %s: %s", resp.Status, body)
}

// --- APNs (iOS), token-based, HTTP/2 ---------------------------------------

type apnsSender struct {
	topic   string // app bundle id
	signer  *apnsSigner
	baseURL string // https://api.push.apple.com, overridable in tests
	client  *http.Client
}

func (s *apnsSender) send(ctx context.Context, deviceToken string, e Event) error {
	jwtTok, err := s.signer.token()
	if err != nil {
		return fmt.Errorf("apns auth: %w", err)
	}
	payload, _ := json.Marshal(map[string]any{
		"aps": map[string]any{
			"alert": map[string]string{
				"title": "Approval needed",
				"body":  "An agent is requesting your authorization.",
			},
		},
		"type":             e.Type,
		"consentRequestId": e.ConsentRequestID,
		"agentId":          e.AgentID,
	})
	url := fmt.Sprintf("%s/3/device/%s", s.baseURL, deviceToken)
	req, _ := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(payload))
	req.Header.Set("authorization", "bearer "+jwtTok)
	req.Header.Set("apns-topic", s.topic)
	req.Header.Set("apns-push-type", "alert")
	// A consent prompt is only useful while the request is pending. Cap delivery
	// so a brief-offline phone still gets it, but APNs drops it rather than
	// surfacing a stale prompt for an already-resolved request hours later.
	req.Header.Set("apns-expiration", strconv.FormatInt(time.Now().Add(5*time.Minute).Unix(), 10))
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		return nil
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	// APNs reports a dead token as 410 Gone, or 400 with reason BadDeviceToken.
	if resp.StatusCode == http.StatusGone || bytes.Contains(body, []byte("BadDeviceToken")) {
		return ErrDeviceUnregistered
	}
	return fmt.Errorf("apns send: %s: %s", resp.Status, body)
}

// --- Google service-account access token (JWT-bearer flow) ------------------

// googleTokenSource mints and caches an FCM access token from a service-account
// key using the JWT-bearer grant, avoiding the google.golang.org/api dependency.
type googleTokenSource struct {
	clientEmail string
	privateKey  *rsa.PrivateKey
	tokenURI    string
	client      *http.Client

	mu     sync.Mutex
	cached string
	expiry time.Time
}

const fcmScope = "https://www.googleapis.com/auth/firebase.messaging"

func (g *googleTokenSource) token(ctx context.Context) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.cached != "" && time.Now().Before(g.expiry.Add(-1*time.Minute)) {
		return g.cached, nil
	}
	now := time.Now()
	tok, err := jwt.NewBuilder().
		Issuer(g.clientEmail).
		Audience([]string{g.tokenURI}).
		Claim("scope", fcmScope).
		IssuedAt(now).
		Expiration(now.Add(time.Hour)).
		Build()
	if err != nil {
		return "", err
	}
	signed, err := jwt.Sign(tok, jwt.WithKey(jwa.RS256, g.privateKey))
	if err != nil {
		return "", err
	}
	form := fmt.Sprintf("grant_type=urn:ietf:params:oauth:grant-type:jwt-bearer&assertion=%s", string(signed))
	req, _ := http.NewRequestWithContext(ctx, "POST", g.tokenURI, bytes.NewReader([]byte(form)))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := g.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return "", fmt.Errorf("google token: %s: %s", resp.Status, body)
	}
	var out struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	g.cached = out.AccessToken
	g.expiry = now.Add(time.Duration(out.ExpiresIn) * time.Second)
	return g.cached, nil
}

// --- APNs provider JWT (ES256) ----------------------------------------------

// apnsSigner mints and caches the APNs provider authentication token (ES256),
// refreshed well within Apple's ~1h validity window.
type apnsSigner struct {
	keyID  string
	teamID string
	key    *ecdsa.PrivateKey

	mu     sync.Mutex
	cached string
	issued time.Time
}

func (a *apnsSigner) token() (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	// Apple rejects tokens older than 1h and recommends refreshing no more than
	// once every 20m; refresh at 50m.
	if a.cached != "" && time.Since(a.issued) < 50*time.Minute {
		return a.cached, nil
	}
	now := time.Now()
	tok, err := jwt.NewBuilder().Issuer(a.teamID).IssuedAt(now).Build()
	if err != nil {
		return "", err
	}
	// APNs requires the provider token's JWS header to carry the key id (kid);
	// alg=ES256 is set by the signer.
	hdrs := jws.NewHeaders()
	if err := hdrs.Set(jws.KeyIDKey, a.keyID); err != nil {
		return "", err
	}
	signed, err := jwt.Sign(tok, jwt.WithKey(jwa.ES256, a.key, jws.WithProtectedHeaders(hdrs)))
	if err != nil {
		return "", err
	}
	a.cached = string(signed)
	a.issued = now
	return a.cached, nil
}

// parseRSAKey parses a PEM PKCS#8 RSA private key (the service-account key form).
func parseRSAKey(pemBytes []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, errors.New("no PEM block in RSA key")
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	rk, ok := k.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("key is not RSA")
	}
	return rk, nil
}

// parseECKey parses a PEM PKCS#8 EC private key (the APNs .p8 form).
func parseECKey(pemBytes []byte) (*ecdsa.PrivateKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, errors.New("no PEM block in EC key")
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	ek, ok := k.(*ecdsa.PrivateKey)
	if !ok {
		return nil, errors.New("key is not ECDSA")
	}
	return ek, nil
}
