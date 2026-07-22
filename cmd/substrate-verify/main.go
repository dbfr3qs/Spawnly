// Command substrate-verify runs a substrate workload's JWT-SVID through the
// Phase-1 Go verifier chain and asserts it authenticates as the EXPECTED org —
// the AC 3 / AC 4 check, executed against real code (not a re-implementation).
//
// It is the Go half of scripts/substrate-verify.sh: the script fetches the
// org-scoped JWT-SVID from the substrate SPIRE agent's workload API and pipes it
// here with the expected org. This mirrors exactly what the registry does on a
// self-register call: JWKS-validate the SVID, parse it through the canonical
// path contract (trust-domain enforced), and bind the org segment.
//
// Exit 0 iff the SVID validates, its trust domain matches, and its org segment
// equals -org (a "matching org record"). Exit non-zero otherwise — including the
// negative sanity check that a DIFFERENT org would be rejected (fail closed).
package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"

	"github.com/spawnly/platform/internal/registrant"
	"github.com/spawnly/platform/internal/spiffe"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "substrate-verify: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("OK")
}

func run() error {
	var (
		token       = flag.String("token", "", "the workload JWT-SVID to verify")
		jwksURL     = flag.String("jwks-url", "", "SPIRE OIDC JWKS URL")
		org         = flag.String("org", "", "expected org id (the matching org record's org)")
		trustDomain = flag.String("trust-domain", "cluster.local", "expected trust domain")
	)
	flag.Parse()
	if *token == "" || *jwksURL == "" || *org == "" {
		return fmt.Errorf("-token, -jwks-url and -org are required")
	}

	ctx := context.Background()
	validator, err := spiffe.NewJWKSValidator(ctx, *jwksURL)
	if err != nil {
		return fmt.Errorf("build JWKS validator: %w", err)
	}
	// Trust-domain enforcement ON — exactly the registry's production wiring. The
	// verifier itself asserts the fixed "registry" audience, matching what the
	// script fetches; there is deliberately no audience flag to keep this tool an
	// honest mirror of the registry's self-register path.
	verifier := &registrant.SpiffeVerifier{Validator: validator, TrustDomain: *trustDomain}

	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+*token)

	id, err := verifier.Verify(ctx, req)
	if err != nil {
		return fmt.Errorf("Phase-1 verifier rejected the SVID: %w", err)
	}
	fmt.Fprintf(os.Stderr, "verified: agentID=%q org=%q subject=%q\n", id.AgentID, id.OrgID, id.Subject)

	// AC 3/4: the SVID must carry a non-empty org segment that equals the matching
	// org record's org. A mismatch (incl. an empty/org-less segment) is the
	// isolation failure this tool exists to catch — fail closed.
	if id.OrgID == "" {
		return fmt.Errorf("org-scoped substrate SVID parsed with an EMPTY org segment (expected %q)", *org)
	}
	if id.OrgID != *org {
		return fmt.Errorf("SVID org %q != expected org %q (isolation would bind it to the wrong record)", id.OrgID, *org)
	}
	return nil
}
