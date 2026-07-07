// Command fake-sts is a purpose-built, in-cluster stand-in for AWS STS
// GetCallerIdentity, used ONLY by the e2e attestation suite.
//
// ┌───────────────────────────────────────────────────────────────────────────┐
// │ TRUST-INJECTION POINT — E2E ONLY. NOT A REAL STS.                          │
// │                                                                            │
// │ This service performs NO SigV4 signature validation whatsoever. It maps a │
// │ presented "signing identity" (the access-key id in X-Amz-Credential, or a │
// │ convenience X-Fake-Principal query param) to a preconfigured account+ARN  │
// │ and answers GetCallerIdentity with it. Anyone who can reach it can mint    │
// │ any identity it is configured for. It exists solely so the anchor         │
// │ proof-of-control + Phase-9 verifier paths can be exercised against         │
// │ deterministic identities without real AWS.                                 │
// │                                                                            │
// │ It is deployed ONLY by the e2e path (deploy/manifests/fake-sts.yaml, not  │
// │ in the kustomize base) and is trusted by a verifier ONLY when that         │
// │ verifier is explicitly pointed at it via STS_TRUSTED_ENDPOINT — a config  │
// │ a real deploy never sets. There is no code path that enables it by         │
// │ default. Do NOT add one.                                                    │
// └───────────────────────────────────────────────────────────────────────────┘
package main

import (
	"encoding/json"
	"encoding/xml"
	"log"
	"net/http"
	"os"
	"strings"
)

// principal is the identity the fake attests for a given signing key.
type principal struct {
	Account string `json:"account"`
	Arn     string `json:"arn"`
}

func main() {
	// FAKE_STS_PRINCIPALS is a JSON object mapping a signing-identity key (an AWS
	// access-key id, or an X-Fake-Principal token) to the account+ARN this fake
	// attests for it. e.g.
	//   {"AKIAACME":{"account":"111122223333","arn":"arn:aws:sts::111122223333:assumed-role/acme-agents/agent-1"}}
	principals := map[string]principal{}
	if raw := os.Getenv("FAKE_STS_PRINCIPALS"); raw != "" {
		if err := json.Unmarshal([]byte(raw), &principals); err != nil {
			log.Fatalf("fake-sts: invalid FAKE_STS_PRINCIPALS: %v", err)
		}
	}
	addr := os.Getenv("FAKE_STS_ADDR")
	if addr == "" {
		addr = ":8080"
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("Action") != "GetCallerIdentity" {
			http.Error(w, "unsupported action", http.StatusBadRequest)
			return
		}
		key := principalKey(r)
		p, ok := principals[key]
		if !ok {
			// STS returns 403 for an unknown/invalid caller; the verifier treats any
			// non-200 as a failed proof.
			log.Printf("fake-sts: no principal for signing key %q", key)
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`<ErrorResponse><Error><Code>InvalidClientTokenId</Code></Error></ErrorResponse>`))
			return
		}
		writeIdentity(w, p)
	})
	// Liveness for the k8s readiness probe.
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })

	log.Printf("fake-sts (E2E TRUST-INJECTION STUB) listening on %s with %d principal(s)", addr, len(principals))
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatalf("fake-sts: %v", err)
	}
}

// principalKey derives the signing-identity lookup key from the request:
// preferring the explicit X-Fake-Principal query param (the convenience the TS
// e2e uses), then the access-key id embedded in a real presigned request's
// X-Amz-Credential (the realistic anchorctl path). Returns "" if neither is
// present, which never matches a configured principal (fail-closed).
func principalKey(r *http.Request) string {
	q := r.URL.Query()
	if k := q.Get("X-Fake-Principal"); k != "" {
		return k
	}
	// X-Amz-Credential is "<AKID>/<date>/<region>/sts/aws4_request".
	cred := q.Get("X-Amz-Credential")
	if cred == "" {
		cred = firstCredentialFromAuth(r.Header.Get("Authorization"))
	}
	if cred == "" {
		return ""
	}
	return strings.SplitN(cred, "/", 2)[0]
}

// firstCredentialFromAuth pulls the Credential=<AKID>/... value out of a SigV4
// Authorization header, for the rare case a client signs with headers rather
// than query params.
func firstCredentialFromAuth(auth string) string {
	const marker = "Credential="
	i := strings.Index(auth, marker)
	if i < 0 {
		return ""
	}
	rest := auth[i+len(marker):]
	if j := strings.IndexAny(rest, ", "); j >= 0 {
		return rest[:j]
	}
	return rest
}

func writeIdentity(w http.ResponseWriter, p principal) {
	// Namespace-free XML: the Go and Java STS response parsers both match by
	// element LOCAL name, so the doc namespace real STS emits is irrelevant, and
	// omitting it sidesteps encoder namespace-inheritance quirks.
	type result struct {
		Arn     string `xml:"Arn"`
		UserID  string `xml:"UserId"`
		Account string `xml:"Account"`
	}
	type response struct {
		XMLName xml.Name `xml:"GetCallerIdentityResponse"`
		Result  result   `xml:"GetCallerIdentityResult"`
	}
	w.Header().Set("Content-Type", "text/xml")
	_, _ = w.Write([]byte(xml.Header))
	_ = xml.NewEncoder(w).Encode(response{
		Result: result{Arn: p.Arn, UserID: "AROAFAKE:" + p.Account, Account: p.Account},
	})
}
