package attestor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestEncodeDecodePresigned_RoundTrip(t *testing.T) {
	in := presignedRequest{
		Method:  "POST",
		URL:     "https://sts.amazonaws.com/?Action=GetCallerIdentity&Version=2011-06-15",
		Headers: map[string][]string{"X-Amz-Date": {"20260617T000000Z"}},
	}
	enc, err := encodePresigned(in)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	out, err := decodePresigned(enc)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Method != in.Method || out.URL != in.URL || out.Headers["X-Amz-Date"][0] != "20260617T000000Z" {
		t.Fatalf("round-trip mismatch: %+v", out)
	}
}

func TestAwsSource_Fetch(t *testing.T) {
	s := &AwsSource{presign: func(context.Context) (presignedRequest, error) {
		return presignedRequest{Method: "POST", URL: "https://sts.amazonaws.com/?Action=GetCallerIdentity"}, nil
	}}
	cred, err := s.Fetch(context.Background(), "ignored-audience")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if cred.AssertionType != AwsStsAssertionType {
		t.Fatalf("AssertionType = %q, want %q", cred.AssertionType, AwsStsAssertionType)
	}
	if _, err := decodePresigned(cred.Value); err != nil {
		t.Fatalf("credential value is not a decodable presigned request: %v", err)
	}
}

func TestValidateStsURL(t *testing.T) {
	cases := map[string]bool{
		"https://sts.amazonaws.com/?Action=GetCallerIdentity":           true,
		"https://sts.us-east-1.amazonaws.com/?Action=GetCallerIdentity": true,
		"http://sts.amazonaws.com/?Action=GetCallerIdentity":            false, // not https
		"https://evil.example.com/?Action=GetCallerIdentity":            false, // not STS
		"https://sts.amazonaws.com/?Action=AssumeRole":                  false, // wrong action
		"https://sts.amazonaws.com.evil.com/?Action=GetCallerIdentity":  false, // host suffix attack
	}
	for raw, wantOK := range cases {
		err := validateStsURL(raw)
		if wantOK && err != nil {
			t.Errorf("validateStsURL(%q) = %v, want ok", raw, err)
		}
		if !wantOK && err == nil {
			t.Errorf("validateStsURL(%q) = ok, want error", raw)
		}
	}
}

func TestMatchRoleArnPattern(t *testing.T) {
	arn := "arn:aws:sts::111122223333:assumed-role/acme-agents/agent-1"
	cases := []struct {
		pattern string
		want    bool
	}{
		{"arn:aws:sts::111122223333:assumed-role/acme-agents/*", true},
		{"arn:aws:sts::111122223333:assumed-role/acme-agents/agent-1", true}, // exact
		{"arn:aws:sts::111122223333:assumed-role/*/agent-1", true},
		{"*", true}, // matches, but registration rejects a bare "*"
		{"", false}, // empty pattern fails closed
		{"arn:aws:sts::999988887777:assumed-role/acme-agents/*", false}, // other account
		{"arn:aws:sts::111122223333:assumed-role/other/*", false},       // other role
		{"arn:aws:sts::111122223333:assumed-role/acme-agents", false},   // no trailing session
	}
	for _, c := range cases {
		if got := MatchRoleArnPattern(c.pattern, arn); got != c.want {
			t.Errorf("MatchRoleArnPattern(%q, arn) = %v, want %v", c.pattern, got, c.want)
		}
	}
	// A `*` in the ARN must not be treated as a metacharacter on the ARN side.
	if MatchRoleArnPattern("arn:aws:sts::1:assumed-role/r/a", "arn:aws:sts::1:assumed-role/r/*") {
		t.Error("literal '*' in ARN should not match pattern 'a'")
	}
}

func TestSessionNameFromArn(t *testing.T) {
	got, err := SessionNameFromArn("arn:aws:sts::123456789012:assumed-role/spawnly-agent/worker-ab12cd")
	if err != nil || got != "worker-ab12cd" {
		t.Fatalf("SessionNameFromArn = %q, %v; want worker-ab12cd, nil", got, err)
	}
	if _, err := SessionNameFromArn("arn:aws:iam::123456789012:user/bob"); err == nil {
		t.Fatal("expected error for non-assumed-role ARN")
	}
}

func TestVerifyGetCallerIdentity(t *testing.T) {
	const arn = "arn:aws:sts::123456789012:assumed-role/spawnly-agent/worker-ab12cd"
	const account = "123456789012"
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/xml")
		_, _ = w.Write([]byte(`<GetCallerIdentityResponse><GetCallerIdentityResult>` +
			`<Arn>` + arn + `</Arn><Account>` + account + `</Account>` +
			`</GetCallerIdentityResult></GetCallerIdentityResponse>`))
	}))
	defer srv.Close()

	client := srv.Client()

	// Happy path: replay against the (test) STS endpoint and parse account + ARN.
	pr := presignedRequest{Method: "GET", URL: srv.URL + "/?Action=GetCallerIdentity"}
	id, err := replayCallerIdentity(context.Background(), client, pr)
	if err != nil || id.Arn != arn || id.Account != account {
		t.Fatalf("replayCallerIdentity = %+v, %v; want arn=%q account=%q", id, err, arn, account)
	}

	// SSRF guard: the full verify path rejects a non-STS host.
	cred, _ := encodePresigned(pr)
	if _, err := VerifyGetCallerIdentity(context.Background(), client, cred); err == nil ||
		!strings.Contains(err.Error(), "not an AWS STS endpoint") {
		t.Fatalf("expected STS-host rejection, got %v", err)
	}
}

// TestVerifyCallerIdentity_AccountFallback proves the account is derived from the
// ARN when the STS response omits the <Account> element.
func TestVerifyCallerIdentity_AccountFallback(t *testing.T) {
	const arn = "arn:aws:sts::444455556666:assumed-role/acme-agents/agent-1"
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`<GetCallerIdentityResponse><GetCallerIdentityResult>` +
			`<Arn>` + arn + `</Arn></GetCallerIdentityResult></GetCallerIdentityResponse>`))
	}))
	defer srv.Close()
	id, err := replayCallerIdentity(context.Background(), srv.Client(), presignedRequest{
		Method: "GET", URL: srv.URL + "/?Action=GetCallerIdentity",
	})
	if err != nil || id.Account != "444455556666" {
		t.Fatalf("account fallback = %+v, %v; want 444455556666", id, err)
	}
}

// TestValidateStsURL_TrustedEndpoint proves the STS_TRUSTED_ENDPOINT override
// NARROWS trust to exactly the configured endpoint (never widens it): with the
// override set only that scheme+host is accepted — plain http to the fake is
// allowed, but the real AWS pattern and any other host are refused.
func TestValidateStsURL_TrustedEndpoint(t *testing.T) {
	const trusted = "http://fake-sts:8080"
	cases := map[string]bool{
		"http://fake-sts:8080/?Action=GetCallerIdentity":      true,  // exact match, http ok when trusted
		"http://fake-sts:8080/foo?Action=GetCallerIdentity":   true,  // path is irrelevant
		"https://fake-sts:8080/?Action=GetCallerIdentity":     false, // scheme differs
		"http://fake-sts:9090/?Action=GetCallerIdentity":      false, // port differs
		"http://other:8080/?Action=GetCallerIdentity":         false, // host differs
		"https://sts.amazonaws.com/?Action=GetCallerIdentity": false, // real AWS no longer trusted
		"http://fake-sts:8080/?Action=AssumeRole":             false, // wrong action
	}
	for raw, wantOK := range cases {
		err := validateStsURLWith(raw, trusted)
		if wantOK && err != nil {
			t.Errorf("validateStsURLWith(%q, trusted) = %v, want ok", raw, err)
		}
		if !wantOK && err == nil {
			t.Errorf("validateStsURLWith(%q, trusted) = ok, want error", raw)
		}
	}
	// Unset override keeps the real AWS default (https + STS host pattern).
	if err := validateStsURLWith("https://sts.amazonaws.com/?Action=GetCallerIdentity", ""); err != nil {
		t.Errorf("default (no override) rejected real STS: %v", err)
	}
	if err := validateStsURLWith("http://fake-sts:8080/?Action=GetCallerIdentity", ""); err == nil {
		t.Error("default (no override) accepted fake endpoint, want reject")
	}
}
