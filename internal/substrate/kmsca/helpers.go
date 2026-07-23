package kmsca

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
)

// loadConfig resolves AWS configuration from the default credential chain (EKS Pod
// Identity on the edge). Kept behind a package var so this stays the single place
// the edge KMS client obtains credentials.
var loadConfig = func(ctx context.Context) (aws.Config, error) {
	return config.LoadDefaultConfig(ctx)
}

// countCertBlocks counts CERTIFICATE PEM blocks in pemBytes (other block types
// are not counted here — parseCert/assert paths reject them separately).
func countCertBlocks(pemBytes []byte) int {
	n := 0
	for {
		var b *pem.Block
		b, pemBytes = pem.Decode(pemBytes)
		if b == nil {
			return n
		}
		if b.Type == "CERTIFICATE" {
			n++
		}
	}
}

// parseCert decodes the first CERTIFICATE PEM block to an x509 certificate.
func parseCert(certPEM []byte) (*x509.Certificate, error) {
	b, _ := pem.Decode(certPEM)
	if b == nil {
		return nil, errors.New("no CERTIFICATE PEM block")
	}
	if b.Type != "CERTIFICATE" {
		return nil, errors.New("PEM block is not a CERTIFICATE")
	}
	return x509.ParseCertificate(b.Bytes)
}
