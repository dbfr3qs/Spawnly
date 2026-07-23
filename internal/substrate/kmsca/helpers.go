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
