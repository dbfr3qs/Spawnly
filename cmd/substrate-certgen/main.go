// Command substrate-certgen is the LOCAL-SIM control-plane half of x509pop
// identity issuance. It has two modes, both writing PEM to files (never stdout,
// so key material doesn't land in shell history / logs):
//
//	-mode ca        generate a throwaway dev CA (cert + key)
//	-mode leaf      sign an org-bound leaf off an existing dev CA
//
// It is the Phase-2 stand-in for Phase-3's CP-mediated issuance (the connect
// API): scripts/substrate-ca.sh calls -mode ca, scripts/substrate-dev-cert.sh
// calls -mode leaf. All crypto lives in internal/substrate/devca so it is
// unit-tested; this is a thin, side-effecting shell. NONE of the material it
// writes is ever committed or baked into an image (see devca package doc).
package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/spawnly/platform/internal/substrate/devca"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "substrate-certgen: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		mode        = flag.String("mode", "", "ca | leaf")
		trustDomain = flag.String("trust-domain", "cluster.local", "SPIFFE trust domain")
		orgID       = flag.String("org", "", "org id (leaf mode): stamped into the CommonName the server template binds on")
		ttl         = flag.Duration("ttl", 0, "certificate validity (default: 720h CA / 168h leaf)")
		caCertIn    = flag.String("ca-cert", "", "path to the dev CA cert PEM (leaf mode)")
		caKeyIn     = flag.String("ca-key", "", "path to the dev CA key PEM (leaf mode)")
		certOut     = flag.String("cert-out", "", "output path for the cert PEM")
		keyOut      = flag.String("key-out", "", "output path for the key PEM")
	)
	flag.Parse()

	switch *mode {
	case "ca":
		d := *ttl
		if d == 0 {
			d = 720 * time.Hour
		}
		if *certOut == "" || *keyOut == "" {
			return fmt.Errorf("-mode ca requires -cert-out and -key-out")
		}
		ca, err := devca.GenerateCA(d)
		if err != nil {
			return err
		}
		keyPEM, err := ca.KeyPEM()
		if err != nil {
			return err
		}
		if err := writeFile(*certOut, ca.CertPEM, 0o644); err != nil {
			return err
		}
		if err := writeFile(*keyOut, keyPEM, 0o600); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "wrote dev CA cert=%s key=%s (valid %s)\n", *certOut, *keyOut, d)
		return nil

	case "leaf":
		d := *ttl
		if d == 0 {
			d = 168 * time.Hour
		}
		if *orgID == "" || *caCertIn == "" || *caKeyIn == "" || *certOut == "" || *keyOut == "" {
			return fmt.Errorf("-mode leaf requires -org, -ca-cert, -ca-key, -cert-out, -key-out")
		}
		caCertPEM, err := os.ReadFile(*caCertIn)
		if err != nil {
			return fmt.Errorf("read ca cert: %w", err)
		}
		caKeyPEM, err := os.ReadFile(*caKeyIn)
		if err != nil {
			return fmt.Errorf("read ca key: %w", err)
		}
		ca, err := devca.LoadCA(caCertPEM, caKeyPEM)
		if err != nil {
			return err
		}
		leaf, err := devca.IssueOrgLeaf(ca, *trustDomain, *orgID, d)
		if err != nil {
			return err
		}
		if err := writeFile(*certOut, leaf.CertPEM, 0o644); err != nil {
			return err
		}
		if err := writeFile(*keyOut, leaf.KeyPEM, 0o600); err != nil {
			return err
		}
		prefix, _ := devca.NodeSpiffePathPrefix(*trustDomain, *orgID)
		fmt.Fprintf(os.Stderr, "wrote org=%q leaf cert=%s key=%s; node will attest under %s<fingerprint>\n",
			*orgID, *certOut, *keyOut, prefix)
		return nil

	default:
		return fmt.Errorf("unknown -mode %q (want ca | leaf)", *mode)
	}
}

func writeFile(path string, data []byte, perm os.FileMode) error {
	if err := os.WriteFile(path, data, perm); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}
