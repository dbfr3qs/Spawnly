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
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/xml")
		_, _ = w.Write([]byte(`<GetCallerIdentityResponse><GetCallerIdentityResult>` +
			`<Arn>` + arn + `</Arn></GetCallerIdentityResult></GetCallerIdentityResponse>`))
	}))
	defer srv.Close()

	client := srv.Client()

	// Happy path: replay against the (test) STS endpoint and parse the ARN.
	pr := presignedRequest{Method: "GET", URL: srv.URL + "/?Action=GetCallerIdentity"}
	got, err := replayGetCallerIdentity(context.Background(), client, pr)
	if err != nil || got != arn {
		t.Fatalf("replayGetCallerIdentity = %q, %v; want %q", got, err, arn)
	}

	// SSRF guard: the full verify path rejects a non-STS host.
	cred, _ := encodePresigned(pr)
	if _, err := VerifyGetCallerIdentity(context.Background(), client, cred); err == nil ||
		!strings.Contains(err.Error(), "not an AWS STS endpoint") {
		t.Fatalf("expected STS-host rejection, got %v", err)
	}
}
