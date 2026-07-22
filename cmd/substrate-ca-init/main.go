// Command substrate-ca-init bootstraps the PRODUCTION-EDGE substrate CA
// certificate for the AWS KMS signing key. It emits ONLY the self-signed CA
// CERTIFICATE (PEM) to stdout — public material Phase 4's deploy.sh stores in a
// ConfigMap. There is no private key to emit: the CA key lives in KMS and is used
// only via `kms:Sign`; this tool never prints, exports, or even holds key material.
//
// It is idempotent: given an existing CA cert (-existing) whose SubjectPublicKeyInfo
// matches the KMS key, it re-emits that cert unchanged, so re-running against a
// stored ConfigMap is a no-op. A mismatched existing cert is a hard error, never a
// silent replacement.
//
// All crypto lives in internal/substrate/kmsca (unit-tested with a fake KMS); this
// is a thin, side-effecting shell. AWS credentials come from the default chain
// (EKS Pod Identity), needing only kms:GetPublicKey + kms:Sign on the one key.
package main

import (
	"context"
	"encoding/pem"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/spawnly/platform/internal/substrate/kmsca"
)

func main() {
	if err := run(context.Background(), os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "substrate-ca-init: %v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, stdout *os.File) error {
	var (
		keyARN     = flag.String("key-arn", os.Getenv("SUBSTRATE_KMS_KEY_ARN"), "AWS KMS key id/ARN/alias for the substrate CA (or SUBSTRATE_KMS_KEY_ARN)")
		existingIn = flag.String("existing", "", "path to an existing CA cert PEM; re-emitted unchanged iff its key matches the KMS key (idempotency)")
		ttl        = flag.Duration("ttl", kmsca.DefaultCATTL, "CA certificate validity when minting a fresh cert")
	)
	flag.Parse()

	if *keyARN == "" {
		return fmt.Errorf("a KMS key is required (-key-arn or SUBSTRATE_KMS_KEY_ARN)")
	}

	var existingPEM []byte
	if *existingIn != "" {
		b, err := os.ReadFile(*existingIn)
		if err != nil {
			return fmt.Errorf("read existing CA cert: %w", err)
		}
		existingPEM = b
	}

	api, err := kmsca.NewSigningClient(ctx)
	if err != nil {
		return err
	}
	signer, err := kmsca.NewSigner(ctx, api, *keyARN)
	if err != nil {
		return err
	}
	certPEM, err := kmsca.BootstrapCACert(signer, existingPEM, *ttl)
	if err != nil {
		return err
	}

	// Defense in depth: NEVER emit anything but CERTIFICATE blocks. If any block is
	// not a CERTIFICATE (there is no path that could produce one — the KMS key is
	// unexportable), refuse rather than risk leaking key-shaped material to stdout.
	if err := assertCertOnly(certPEM); err != nil {
		return err
	}

	if _, err := stdout.Write(certPEM); err != nil {
		return fmt.Errorf("write CA cert: %w", err)
	}
	fmt.Fprintf(os.Stderr, "substrate-ca-init: emitted CA certificate (public) for key %s (valid %s)\n", *keyARN, ttlNote(existingPEM, *ttl))
	return nil
}

func ttlNote(existingPEM []byte, ttl time.Duration) string {
	if len(existingPEM) > 0 {
		return "reused existing"
	}
	return ttl.String()
}

// assertCertOnly verifies pemBytes contains ONLY CERTIFICATE blocks and at least
// one — a hard guarantee no private-key material is ever written to stdout.
func assertCertOnly(pemBytes []byte) error {
	rest := pemBytes
	seen := 0
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			return fmt.Errorf("refusing to emit a non-CERTIFICATE PEM block (%q)", block.Type)
		}
		seen++
	}
	if seen == 0 {
		return fmt.Errorf("no CERTIFICATE block produced")
	}
	return nil
}
