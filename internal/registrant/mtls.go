package registrant

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path"
)

// MTLS agent-id extraction modes.
const (
	MTLSSourceSANURI     = "san_uri"
	MTLSSourceSANDNS     = "san_dns"
	MTLSSourceCommonName = "common_name"
)

// MTLSConfig configures an MTLSVerifier.
type MTLSConfig struct {
	// AgentIDSource selects how Identity.AgentID is derived from the
	// client certificate's leaf: "san_uri" (last path segment of the
	// first URI SAN, e.g. a SPIFFE-style URI), "san_dns" (first DNS SAN),
	// or "common_name" (Subject.CommonName).
	AgentIDSource string
}

// MTLSVerifier derives Identity from the TLS client certificate presented by
// the caller.
//
// This is a sketch-level implementation for Phase 3: the interface and
// config surface are defined now so the verifier-selection mechanism is
// future-proof, but it is not load-bearing until the registry's HTTP server
// is started with http.ListenAndServeTLS and
// tls.Config{ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: <pool>}
// (cmd/registry/main.go currently uses http.ListenAndServe, which never
// populates r.TLS.PeerCertificates). That server/TLS wiring is a follow-up
// change.
type MTLSVerifier struct {
	cfg MTLSConfig
}

// NewMTLSVerifier returns a Verifier that reads identity from the TLS peer
// certificate per cfg.AgentIDSource.
func NewMTLSVerifier(cfg MTLSConfig) *MTLSVerifier {
	return &MTLSVerifier{cfg: cfg}
}

// Verify requires r.TLS.PeerCertificates to be populated (i.e. the server
// negotiated and verified a client certificate) and extracts Identity from
// the leaf certificate per the configured AgentIDSource.
func (v *MTLSVerifier) Verify(_ context.Context, r *http.Request) (Identity, error) {
	if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
		return Identity{}, errors.New("no client certificate presented")
	}
	cert := r.TLS.PeerCertificates[0]

	switch v.cfg.AgentIDSource {
	case "", MTLSSourceSANURI:
		if len(cert.URIs) == 0 {
			return Identity{}, errors.New("client certificate has no URI SAN")
		}
		uri := cert.URIs[0].String()
		return Identity{AgentID: path.Base(uri), Subject: uri, Issuer: "mtls"}, nil
	case MTLSSourceSANDNS:
		if len(cert.DNSNames) == 0 {
			return Identity{}, errors.New("client certificate has no DNS SAN")
		}
		dns := cert.DNSNames[0]
		return Identity{AgentID: dns, Subject: dns, Issuer: "mtls"}, nil
	case MTLSSourceCommonName:
		if cert.Subject.CommonName == "" {
			return Identity{}, errors.New("client certificate has no CommonName")
		}
		cn := cert.Subject.CommonName
		return Identity{AgentID: cn, Subject: cn, Issuer: "mtls"}, nil
	default:
		return Identity{}, fmt.Errorf("unknown mTLS agent id source %q", v.cfg.AgentIDSource)
	}
}
