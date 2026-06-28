package mobilegateway

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func pemPKCS8(t *testing.T, key any) []byte {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
}

func testRSAKeyPEM(t *testing.T) []byte {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return pemPKCS8(t, k)
}

func testECKeyPEM(t *testing.T) []byte {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return pemPKCS8(t, k)
}

// newTestTransport builds an FCMAPNsTransport whose FCM/APNs/token endpoints all
// point at the given stub server, so request shaping is exercised offline.
func newTestTransport(t *testing.T, stubURL string) *FCMAPNsTransport {
	t.Helper()
	rsaKey, err := parseRSAKey(testRSAKeyPEM(t))
	if err != nil {
		t.Fatal(err)
	}
	ecKey, err := parseECKey(testECKeyPEM(t))
	if err != nil {
		t.Fatal(err)
	}
	return &FCMAPNsTransport{
		fcm: &fcmSender{
			projectID: "proj-1",
			tokens:    &googleTokenSource{clientEmail: "svc@proj.iam", privateKey: rsaKey, tokenURI: stubURL + "/token", client: http.DefaultClient},
			baseURL:   stubURL,
			client:    http.DefaultClient,
		},
		apns: &apnsSender{
			topic:   "run.spawnly.app",
			signer:  &apnsSigner{keyID: "KID123", teamID: "TEAM123", key: ecKey},
			baseURL: stubURL,
			client:  http.DefaultClient,
		},
	}
}

func TestFCMAPNs_AndroidRequestShape(t *testing.T) {
	var tokenHits int
	var pushAuth, pushBody, pushPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/token"):
			tokenHits++
			// Assert the JWT-bearer grant shape.
			b, _ := io.ReadAll(r.Body)
			if !strings.Contains(string(b), "grant_type=urn:ietf:params:oauth:grant-type:jwt-bearer") {
				t.Errorf("token request not jwt-bearer: %s", b)
			}
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"access_token":"ya29.fake","expires_in":3600}`))
		default: // FCM send
			pushPath = r.URL.Path
			pushAuth = r.Header.Get("Authorization")
			b, _ := io.ReadAll(r.Body)
			pushBody = string(b)
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{}`))
		}
	}))
	defer srv.Close()

	tr := newTestTransport(t, srv.URL)
	err := tr.Send(context.Background(), Device{ID: "d1", UserID: "alice", Platform: PlatformAndroid, PushToken: "fcm-token-xyz"},
		Event{Type: "consent_pending", ConsentRequestID: "req-1", AgentID: "a1"})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if pushPath != "/v1/projects/proj-1/messages:send" {
		t.Errorf("fcm path = %q", pushPath)
	}
	if pushAuth != "Bearer ya29.fake" {
		t.Errorf("fcm auth = %q, want minted access token", pushAuth)
	}
	if !strings.Contains(pushBody, "fcm-token-xyz") || !strings.Contains(pushBody, "req-1") {
		t.Errorf("fcm body missing token/consentRequestId: %s", pushBody)
	}
	// The push must NOT carry scopes/binding.
	if strings.Contains(strings.ToLower(pushBody), "scope") || strings.Contains(strings.ToLower(pushBody), "binding") {
		t.Errorf("fcm body leaked a secret field: %s", pushBody)
	}
}

func TestFCMAPNs_iOSRequestShape(t *testing.T) {
	var path, auth, topic, body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		auth = r.Header.Get("authorization")
		topic = r.Header.Get("apns-topic")
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	tr := newTestTransport(t, srv.URL)
	err := tr.Send(context.Background(), Device{ID: "d2", UserID: "alice", Platform: PlatformIOS, PushToken: "apns-tok-abc"},
		Event{Type: "consent_pending", ConsentRequestID: "req-2"})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if path != "/3/device/apns-tok-abc" {
		t.Errorf("apns path = %q", path)
	}
	if !strings.HasPrefix(auth, "bearer ") {
		t.Errorf("apns auth = %q, want bearer <jwt>", auth)
	}
	if topic != "run.spawnly.app" {
		t.Errorf("apns-topic = %q", topic)
	}
	if !strings.Contains(body, "req-2") {
		t.Errorf("apns body missing consentRequestId: %s", body)
	}
}

func TestFCMAPNs_UnregisteredPruneSignal(t *testing.T) {
	// FCM 404 and APNs 410 both surface as ErrDeviceUnregistered.
	fcm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/token") {
			w.Write([]byte(`{"access_token":"t","expires_in":3600}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"error":{"status":"NOT_FOUND"}}`))
	}))
	defer fcm.Close()
	tr := newTestTransport(t, fcm.URL)
	androidDev := Device{ID: "d", UserID: "u", Platform: PlatformAndroid, PushToken: "tok"}
	if err := tr.Send(context.Background(), androidDev, Event{ConsentRequestID: "r"}); err != ErrDeviceUnregistered {
		t.Fatalf("fcm 404: got %v, want ErrDeviceUnregistered", err)
	}

	apns := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusGone)
	}))
	defer apns.Close()
	tr2 := newTestTransport(t, apns.URL)
	if err := tr2.Send(context.Background(), Device{ID: "d", UserID: "u", Platform: PlatformIOS, PushToken: "tok"}, Event{ConsentRequestID: "r"}); err != ErrDeviceUnregistered {
		t.Fatalf("apns 410: got %v, want ErrDeviceUnregistered", err)
	}
}

func TestNewFCMAPNsTransport_Validates(t *testing.T) {
	good := FCMAPNsConfig{
		ServiceAccountJSON: mustSAJSON(t),
		APNsKeyP8:          testECKeyPEM(t),
		APNsKeyID:          "K", APNsTeamID: "T", APNsBundleID: "run.spawnly.app",
	}
	if _, err := NewFCMAPNsTransport(good); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	// Missing APNs identifiers → error.
	bad := good
	bad.APNsBundleID = ""
	if _, err := NewFCMAPNsTransport(bad); err == nil {
		t.Fatal("missing bundleId accepted")
	}
	// Malformed service account → error.
	bad2 := good
	bad2.ServiceAccountJSON = []byte(`{}`)
	if _, err := NewFCMAPNsTransport(bad2); err == nil {
		t.Fatal("empty service account accepted")
	}
}

func mustSAJSON(t *testing.T) []byte {
	t.Helper()
	b, _ := json.Marshal(map[string]string{
		"client_email": "svc@proj.iam.gserviceaccount.com",
		"private_key":  string(testRSAKeyPEM(t)),
		"project_id":   "proj-1",
		"token_uri":    "https://oauth2.googleapis.com/token",
	})
	return b
}
