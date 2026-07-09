// Command anchorctl presigns an AWS STS GetCallerIdentity request from the
// caller's ambient AWS credentials and prints the resulting base64 credential to
// stdout. It is the client half of anchor proof-of-control: an org admin runs it
// from a shell that holds the anchored AWS account's credentials, and
// scripts/activate-anchor.sh POSTs the printed credential to the registry's
// activate endpoint (through the orchestrator), which replays it against STS and
// activates the anchor iff the account + role match.
//
// It reuses internal/attestor/aws.go's presign source, so it honours the same
// AWS credential chain as the runtime sidecar AND the AWS_ENDPOINT_URL_STS
// override — the latter lets e2e presign against the in-cluster fake STS. Spawnly
// never sees the AWS credentials, only the presigned presentation.
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/spawnly/platform/internal/attestor"
)

func main() {
	ctx := context.Background()
	src, err := attestor.NewAwsSource(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "anchorctl: load AWS config: %v\n", err)
		os.Exit(1)
	}
	// The audience argument is ignored by the STS source (GetCallerIdentity has
	// no audience); pass empty.
	cred, err := src.Fetch(ctx, "")
	if err != nil {
		fmt.Fprintf(os.Stderr, "anchorctl: presign GetCallerIdentity: %v\n", err)
		os.Exit(1)
	}
	// Just the base64 credential on stdout — the caller wraps it in the activate
	// request body.
	fmt.Println(cred.Value)
}
